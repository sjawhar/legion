package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var githubIssueWriteVerbs = map[string]bool{
	"comment": true, "create": true, "edit": true, "close": true, "reopen": true, "delete": true,
	"pin": true, "unpin": true, "transfer": true, "lock": true, "unlock": true, "develop": true,
}

func ghAPIWriteMethod(args []string) string {
	explicit, body := "", false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-X" || arg == "--method":
			if i+1 < len(args) {
				explicit = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "-X"):
			explicit = strings.TrimPrefix(arg, "-X")
		case strings.HasPrefix(arg, "--method="):
			explicit = strings.TrimPrefix(arg, "--method=")
		case strings.HasPrefix(arg, "-f") || strings.HasPrefix(arg, "-F") || arg == "--raw-field" ||
			strings.HasPrefix(arg, "--raw-field=") || arg == "--field" || strings.HasPrefix(arg, "--field=") ||
			arg == "--input" || strings.HasPrefix(arg, "--input="):
			body = true
		}
	}
	if explicit != "" {
		return strings.ToUpper(explicit)
	}
	if body {
		return httpMethodPost
	}
	return httpMethodGet
}

const (
	httpMethodGet  = "GET"
	httpMethodPost = "POST"
)

func ghIssueWrite(args []string) bool {
	positional := make([]string, 0, len(args))
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
		}
	}
	for i, word := range positional {
		if word == "issue" {
			for _, later := range positional[i+1:] {
				if githubIssueWriteVerbs[later] {
					return true
				}
			}
		}
	}
	if !contains(positional, "api") || ghAPIWriteMethod(args) == httpMethodGet {
		return false
	}
	for _, word := range positional {
		if issueAPIPath(word) {
			return true
		}
	}
	return false
}

func issueAPIPath(value string) bool {
	path := strings.SplitN(value, "?", 2)[0]
	path = strings.SplitN(path, "#", 2)[0]
	return strings.Contains(path, "/issues/") || strings.HasSuffix(path, "/issues")
}

func ghMergeIntent(args []string) bool {
	positional := make([]string, 0, len(args))
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			positional = append(positional, arg)
		}
	}
	for i, word := range positional {
		if word == "pr" && contains(positional[i+1:], "merge") {
			return true
		}
	}
	if contains(positional, "alias") || contains(positional, "extension") {
		return true
	}
	if !contains(positional, "api") {
		return false
	}
	for _, word := range positional {
		path := strings.TrimRight(strings.SplitN(strings.SplitN(word, "?", 2)[0], "#", 2)[0], "/")
		if strings.HasSuffix(path, "/merge") || strings.HasSuffix(path, "/merges") {
			return true
		}
	}
	if !contains(positional, "graphql") && !anyGraphQLEndpoint(positional) {
		return false
	}
	bodies, known := inlineGraphQLBodies(args)
	if !known {
		return true
	}
	for _, body := range bodies {
		if strings.Contains(strings.ToLower(body), "mutation") {
			return true
		}
	}
	return false
}

func anyGraphQLEndpoint(words []string) bool {
	for _, word := range words {
		path := strings.TrimRight(strings.SplitN(strings.SplitN(word, "?", 2)[0], "#", 2)[0], "/")
		if strings.HasSuffix(path, "/graphql") {
			return true
		}
	}
	return false
}

func inlineGraphQLBodies(args []string) ([]string, bool) {
	bodies := []string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		field := ""
		switch {
		case arg == "-f" || arg == "-F" || arg == "--raw-field" || arg == "--field":
			if i+1 >= len(args) {
				return nil, false
			}
			field, i = args[i+1], i+1
		case strings.HasPrefix(arg, "-f") || strings.HasPrefix(arg, "-F"):
			field = strings.TrimPrefix(strings.TrimPrefix(arg, "-f"), "-F")
			field = strings.TrimPrefix(field, "=")
		case strings.HasPrefix(arg, "--raw-field="):
			field = strings.TrimPrefix(arg, "--raw-field=")
		case strings.HasPrefix(arg, "--field="):
			field = strings.TrimPrefix(arg, "--field=")
		case arg == "--input" || strings.HasPrefix(arg, "--input="):
			return nil, false
		}
		if field == "" {
			continue
		}
		key, value, found := strings.Cut(field, "=")
		if found && key == "query" {
			if strings.HasPrefix(value, "@") {
				return nil, false
			}
			bodies = append(bodies, value)
		}
	}
	return bodies, true
}

func contains(words []string, target string) bool {
	for _, word := range words {
		if word == target {
			return true
		}
	}
	return false
}

func runGh(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: legion gh -- <gh arguments>")
		return 2
	}
	if ghMergeIntent(args) {
		fmt.Fprintln(stderr, "Legion never merges a pull request: publish READY (merger role) and let a human merge under the repository's code-owner rule")
		return 1
	}
	if ghIssueWrite(args) {
		issue := os.Getenv("LEGION_ISSUE")
		if issue == "" {
			issue = "the Dispatch issue"
		}
		fmt.Fprintf(stderr, "Legion issues live on Dispatch; use dispatch_message or dispatch_comment on %s\n", issue)
		return 1
	}
	response, err := redeemGrant(ctx, "/legion/v1/gh-token")
	if err != nil {
		fmt.Fprintf(stderr, "legion gh: Unable to redeem LEGION_GRANT: %v\n", err)
		return 1
	}
	defer response.Body.Close()
	var token githubTokenResponse
	if err := json.NewDecoder(response.Body).Decode(&token); err != nil || token.Token == "" {
		fmt.Fprintln(stderr, "legion gh: daemon returned an invalid GitHub credential response")
		return 1
	}
	env := childEnvironment(token.Token)
	command := exec.CommandContext(ctx, "gh", args...)
	command.Env, command.Stdout, command.Stderr = env, stdout, stderr
	if err := command.Run(); err != nil {
		if status, ok := err.(*exec.ExitError); ok {
			return status.ExitCode()
		}
		fmt.Fprintf(stderr, "legion gh: %v\n", err)
		return 1
	}
	return 0
}

func childEnvironment(token string) []string {
	values := make(map[string]string)
	for _, pair := range os.Environ() {
		name, value, found := strings.Cut(pair, "=")
		if found {
			values[name] = value
		}
	}
	delete(values, "GITHUB_TOKEN")
	delete(values, "LEGION_GRANT")
	delete(values, "LEGION_GRANT_FILE")
	values["GH_TOKEN"] = token
	values["PATH"] = workerBinFreePath(values["PATH"])
	if stateDir := values["LEGION_STATE_DIR"]; stateDir != "" {
		values["GH_CONFIG_DIR"] = filepath.Join(stateDir, "gh")
	}
	out := make([]string, 0, len(values))
	for name, value := range values {
		out = append(out, name+"="+value)
	}
	return out
}
