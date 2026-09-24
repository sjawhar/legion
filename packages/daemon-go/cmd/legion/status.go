package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

func runIssueStatus(ctx context.Context, issue, status string, stdout, stderr io.Writer) int {
	if status != "todo" && status != "backlog" && status != "icebox" {
		fmt.Fprintln(stderr, "legion status: issue status must be todo, backlog, or icebox")
		return 2
	}
	grant, err := grantFromEnvironment()
	if err != nil {
		fmt.Fprintf(stderr, "legion status: %v\n", err)
		return 1
	}
	body, err := json.Marshal(map[string]string{"grantId": grant, "issue": issue, "status": status})
	if err != nil {
		fmt.Fprintf(stderr, "legion status: encode request: %v\n", err)
		return 1
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, daemonURL()+"/legion/v1/issues/status", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(stderr, "legion status: %v\n", err)
		return 1
	}
	request.Header.Set("Content-Type", "application/json")
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
	if len(answer) > 0 {
		_, _ = stdout.Write(answer)
		if answer[len(answer)-1] != '\n' {
			_, _ = fmt.Fprintln(stdout)
		}
	}
	return 0
}
