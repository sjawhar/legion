package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime/workerbin"
)

type grantCredentialRequest struct {
	GrantID string `json:"grantId"`
}

type githubTokenResponse struct {
	Token    string `json:"token"`
	AppLogin string `json:"appLogin"`
	// LegionAppLogins is each Legion role App's login, keyed by its App role ("implement",
	// "review"), which gh-token names beside the caller's; absent when the daemon could not read
	// every one.
	LegionAppLogins map[string]string `json:"legionAppLogins"`
}

// errNoGrant is grantFromEnvironment's refusal when neither the pane's grant file pointer nor the
// manual LEGION_GRANT is set, which a command with a way to run without a grant names.
var errNoGrant = errors.New("LEGION_GRANT_FILE is missing (and LEGION_GRANT is unset)")

// grantFromEnvironment follows the pane contract exactly: a set grant-file pointer is
// authoritative, even if unreadable or blank; LEGION_GRANT is only the manual fallback when the
// pointer is absent.
func grantFromEnvironment() (string, error) {
	if pointer, set := os.LookupEnv("LEGION_GRANT_FILE"); set {
		return config.ReadSecretPointer("LEGION_GRANT_FILE", pointer)
	}
	if grant := strings.TrimSpace(os.Getenv("LEGION_GRANT")); grant != "" {
		return grant, nil
	}
	return "", errNoGrant
}

func daemonURL() string {
	if endpoint := strings.TrimRight(os.Getenv("LEGION_DAEMON_URL"), "/"); endpoint != "" {
		return endpoint
	}
	port := os.Getenv("LEGION_DAEMON_PORT")
	if port == "" {
		port = "13370"
	}
	return "http://127.0.0.1:" + port
}

func redeemGrant(ctx context.Context, route string) (*http.Response, error) {
	grant, err := grantFromEnvironment()
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(grantCredentialRequest{GrantID: grant})
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, daemonURL()+route, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return nil, fmt.Errorf("daemon returned %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	return response, nil
}

// runCredential speaks git's credential-helper protocol. Git calls the helper with get, then
// may invoke store or erase after it has used the one-command credential; those two operations
// must be successful no-ops so the helper never persists an installation token.
func runCredential(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("credential", "usage: legion credential [get|store|erase]", stderr)
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}
	action := "get"
	if flags.NArg() == 1 {
		action = flags.Arg(0)
	}
	if flags.NArg() > 1 || (action != "get" && action != "store" && action != "erase") {
		flags.Usage()
		return 2
	}
	if _, err := io.ReadAll(os.Stdin); err != nil {
		fmt.Fprintf(stderr, "legion credential: read credential request: %v\n", err)
		return 1
	}
	if action != "get" {
		return 0
	}
	response, err := redeemGrant(ctx, "/legion/v1/git-credential")
	if err != nil {
		fmt.Fprintf(stderr, "legion credential: Unable to redeem LEGION_GRANT: %v\n", err)
		return 1
	}
	defer response.Body.Close()
	credential, err := io.ReadAll(response.Body)
	if err != nil {
		fmt.Fprintf(stderr, "legion credential: read daemon response: %v\n", err)
		return 1
	}
	if !bytes.HasPrefix(credential, []byte("username=")) || !bytes.Contains(credential, []byte("\npassword=")) {
		fmt.Fprintln(stderr, "legion credential: daemon returned an invalid git credential response")
		return 1
	}
	if len(credential) == 0 || credential[len(credential)-1] != '\n' {
		credential = append(credential, '\n')
	}
	_, _ = stdout.Write(credential)
	return 0
}

func workerBinFreePath(value string) string {
	entries := make([]string, 0, len(filepath.SplitList(value)))
	for _, entry := range filepath.SplitList(value) {
		if entry != "" && filepath.Base(entry) != workerbin.DirName {
			entries = append(entries, entry)
		}
	}
	return strings.Join(entries, string(filepath.ListSeparator))
}
