package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/config"
)

const issueStatusUsage = "usage: legion status <issue> todo|backlog|icebox [--operator-token-file <file>] [--config <legion.yaml> | --port <port>]"

// runIssueStatus is `legion status <issue> <status>`, the controller's capability: Dispatch moves
// the issue to one of the three statuses a controller sets, through the daemon's issue status
// route, which takes a controller grant. Inside a controller session the grant is the one the
// extension wrote to LEGION_GRANT_FILE for this command. From an operator's shell,
// --operator-token-file presents the operator's bearer to the grants route's operator form for a
// grant of its own; the bearer is only ever the file's contents, in no argv and no output. The
// daemon is the one LEGION_DAEMON_URL names, or --config / --port's, found as `legion state` finds
// it.
func runIssueStatus(ctx context.Context, issue, status string, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("status", stderr)
	tokenFile := flags.String("operator-token-file", "", "file holding the operator bearer; mints the controller grant from an operator shell")
	configPath := flags.String("config", "", "path to legion.yaml, to find the daemon")
	port := flags.Int("port", 0, "port to reach, overriding the configured one")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 || (status != "todo" && status != "backlog" && status != "icebox") {
		fmt.Fprintln(stderr, issueStatusUsage)
		return 2
	}
	var bearer string
	if *tokenFile != "" {
		read, err := config.ReadSecretPointer("--operator-token-file", *tokenFile)
		if err != nil {
			fmt.Fprintf(stderr, "legion status: %v\n", err)
			return 1
		}
		bearer = read
	}
	address := daemonURL()
	if *configPath != "" || *port != 0 {
		configured, err := stateAddress(*configPath, *port)
		if err != nil {
			fmt.Fprintf(stderr, "legion status: %v\n", err)
			return 1
		}
		address = "http://" + configured
	}

	var grant string
	if bearer != "" {
		var minted api.GrantResponse
		if code := postDaemon(ctx, address+"/legion/v1/grants", bearer, struct{}{}, stderr, func(answer []byte) error {
			if err := json.Unmarshal(answer, &minted); err != nil || minted.GrantID == "" {
				return fmt.Errorf("the daemon answered the grant request without a grant: %s", answer)
			}
			return nil
		}); code != 0 {
			return code
		}
		grant = minted.GrantID
	} else {
		read, err := grantFromEnvironment()
		if err != nil {
			fmt.Fprintf(stderr, "legion status: %v\n", err)
			return 1
		}
		grant = read
	}
	return postDaemon(ctx, address+"/legion/v1/issues/status", "", api.IssueStatusRequest{GrantID: grant, Issue: issue, Status: status}, stderr, func(answer []byte) error {
		if len(answer) > 0 {
			_, _ = stdout.Write(answer)
			if answer[len(answer)-1] != '\n' {
				_, _ = fmt.Fprintln(stdout)
			}
		}
		return nil
	})
}

// postDaemon posts body as JSON to url — with bearer as its Authorization when not "" — and hands
// a 2xx answer to accept; a refusal is printed with its status and body, and the command fails.
func postDaemon(ctx context.Context, url, bearer string, body any, stderr io.Writer, accept func([]byte) error) int {
	encoded, err := json.Marshal(body)
	if err != nil {
		fmt.Fprintf(stderr, "legion status: encode request: %v\n", err)
		return 1
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		fmt.Fprintf(stderr, "legion status: %v\n", err)
		return 1
	}
	request.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		fmt.Fprintf(stderr, "legion status: %v\n", err)
		return 1
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		fmt.Fprintf(stderr, "legion status: read daemon response: %v\n", err)
		return 1
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		fmt.Fprintf(stderr, "legion status: daemon returned %d: %s\n", response.StatusCode, string(answer))
		return 1
	}
	if err := accept(answer); err != nil {
		fmt.Fprintf(stderr, "legion status: %v\n", err)
		return 1
	}
	return 0
}
