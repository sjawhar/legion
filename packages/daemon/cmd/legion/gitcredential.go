package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/ghconfig"
)

const gitCredentialUsage = "usage: legion git-credential get|store|erase  (git's credential helper for a shared clone; git runs it, never a person)"

// gitCredentialStdin is where `legion git-credential` reads git's credential request: the
// process's stdin, which a test replaces.
var gitCredentialStdin io.Reader = os.Stdin

// runGitCredential is every shared clone's credential helper (workspace.GitHubCredentialHelper),
// which git runs with its operation and the request on stdin. `get` answers a request for
// https://github.com with the installation-token username and the token of the owner the
// request's path names (credential.useHttpPath, which every shared clone sets): the hosts.yml in
// that owner's directory of LEGION_GH_OWNERS_DIR when the bundle holds the owner, and otherwise
// the one under GH_CONFIG_DIR, the issue's owner's. Any other host or protocol, an unset
// GH_CONFIG_DIR, or a hosts.yml with no token answers nothing and exits 1, and git, which every
// shared clone tells credential.interactive=false, then fails rather than prompts. `store` and
// `erase` change nothing: the daemon is the one writer of the gh files.
func runGitCredential(_ context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && helpRequested(args[0]) {
		fmt.Fprintln(stderr, gitCredentialUsage)
		return 0
	}
	if len(args) != 1 {
		fmt.Fprintln(stderr, gitCredentialUsage)
		return 2
	}
	switch args[0] {
	case "store", "erase":
		_, _ = io.Copy(io.Discard, gitCredentialStdin)
		return 0
	case "get":
	default:
		fmt.Fprintln(stderr, gitCredentialUsage)
		return 2
	}
	request := readCredentialRequest(gitCredentialStdin)
	if request["protocol"] != "https" {
		fmt.Fprintf(stderr, "legion git-credential: protocol %q is not https; answering nothing\n", request["protocol"])
		return 1
	}
	if request["host"] != "github.com" {
		fmt.Fprintf(stderr, "legion git-credential: host %q is not github.com; answering nothing\n", request["host"])
		return 1
	}
	defaultDir := os.Getenv("GH_CONFIG_DIR")
	if defaultDir == "" {
		fmt.Fprintln(stderr, "legion git-credential: GH_CONFIG_DIR is unset; answering nothing")
		return 1
	}
	owner, _, _ := strings.Cut(strings.TrimPrefix(request["path"], "/"), "/")
	dir := ownerGHConfigDir(os.Getenv("LEGION_GH_OWNERS_DIR"), defaultDir, owner)
	hosts, err := os.ReadFile(filepath.Join(dir, ghconfig.HostsFile))
	if err != nil {
		fmt.Fprintf(stderr, "legion git-credential: %v; answering nothing\n", err)
		return 1
	}
	token, err := ghconfig.TokenFromHosts(hosts)
	if err != nil {
		fmt.Fprintf(stderr, "legion git-credential: %s: %v; answering nothing\n", dir, err)
		return 1
	}
	fmt.Fprintf(stdout, "username=%s\npassword=%s\n", ghconfig.GitUser, token)
	return 0
}

// readCredentialRequest reads git's credential protocol: `key=value` lines up to a blank line or
// the end of input, the last value of a key winning.
func readCredentialRequest(r io.Reader) map[string]string {
	request := map[string]string{}
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		if key, value, ok := strings.Cut(line, "="); ok {
			request[key] = value
		}
	}
	return request
}

// ownerGHConfigDir is the gh directory for owner: its directory of the bundle under ownersDir when
// owner, lowercased, is a login the bundle holds, and defaultDir — the issue's owner's, which
// GH_CONFIG_DIR names — otherwise, an empty owner and an unset ownersDir included.
func ownerGHConfigDir(ownersDir, defaultDir, owner string) string {
	owner = strings.ToLower(owner)
	if ownersDir == "" || !ghconfig.IsOwner(owner) {
		return defaultDir
	}
	dir := ghconfig.OwnerDir(ownersDir, owner)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return defaultDir
	}
	return dir
}
