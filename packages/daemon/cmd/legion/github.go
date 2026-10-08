package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sjawhar/legion/daemon/internal/ghconfig"
)

// errNoGhConfigDir is roleGitHubToken's refusal where nothing names a role's gh files: a session
// no Legion pane or pod started, which a command with a way to run without them hints at.
var errNoGhConfigDir = errors.New("GH_CONFIG_DIR is unset; a Legion pane or pod names the directory of its role's gh files")

// roleGitHubToken is the GitHub App token of the role running the command: the github.com token of
// gh's hosts.yml in the directory GH_CONFIG_DIR names, which the daemon renders from the role's
// lease and rewrites in place as the lease turns over, and which the role's plain gh and git read
// too (internal/ghconfig). Reading the same file keeps the command on the credential the role's
// own gh runs with, and a file that is missing or holds no token is named by its path.
func roleGitHubToken() (string, error) {
	dir := os.Getenv("GH_CONFIG_DIR")
	if dir == "" {
		return "", errNoGhConfigDir
	}
	path := filepath.Join(dir, ghconfig.HostsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	token, err := ghconfig.TokenFromHosts(data)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return token, nil
}
