// The grant is a worker's authentication to the daemon and nothing more: `legion handoff complete`,
// the reviewer's `legion threads resolve` (POST /legion/v1/threads/resolve) and the controller's
// `legion status` send it, and the daemon answers them for the claim it minted it for. No GitHub
// token is redeemed from it: a role's gh and git read its App token from the gh files GH_CONFIG_DIR
// names, which the daemon renders and refreshes in place (roleGitHubToken, internal/ghconfig).

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
	"strings"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// errNoGrant is grantFromEnvironment's refusal when neither the pane's grant file pointer nor the
// manual LEGION_GRANT is set.
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

// postDaemon POSTs payload, as JSON, to the daemon's route, and returns the daemon's answer, whose
// body the caller closes. An answer outside 2xx is an error naming the status and the daemon's own
// words.
func postDaemon(ctx context.Context, route string, payload any) (*http.Response, error) {
	body, err := json.Marshal(payload)
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
		answer, err := io.ReadAll(response.Body)
		if err != nil {
			return nil, fmt.Errorf("daemon returned %d, and reading its answer failed: %w", response.StatusCode, err)
		}
		return nil, fmt.Errorf("daemon returned %d: %s", response.StatusCode, strings.TrimSpace(string(answer)))
	}
	return response, nil
}
