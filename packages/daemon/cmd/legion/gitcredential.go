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

// gitCredentialOperationUsage is each operation's own usage, which its --help prints.
var gitCredentialOperationUsage = map[string]string{
	"get":   "usage: legion git-credential get  (reads git's request on stdin; answers the token of the owner its path names, or nothing, exit 1)",
	"store": "usage: legion git-credential store  (reads git's request on stdin and writes nothing: the daemon is the one writer of the gh files)",
	"erase": "usage: legion git-credential erase  (reads git's request on stdin and erases nothing: the daemon is the one writer of the gh files)",
}

// gitCredentialStdin is where `legion git-credential` reads git's credential request: the
// process's stdin, which a test replaces.
var gitCredentialStdin io.Reader = os.Stdin

// runGitCredential is every shared clone's credential helper (workspace.GitHubCredentialHelper),
// which git runs with its operation and the request on stdin. `get` answers a request for
// https://github.com with the installation-token username and the token of the repository the
// request's path names (credential.useHttpPath, which every shared clone sets): its first segment
// is the owner and its second the repository, and the token comes from the hosts.yml in that
// repository's own directory of LEGION_GH_OWNERS_DIR, `<owner>/<repo>`, where the bundle holds
// one, from the owner's directory otherwise (repositoryGHConfigDir), and from nothing else. A
// repository the bundle holds no directory for — the one the role writes to, every repository of
// an owner it writes to none of, and a name no bundle directory can hold — is the owner's
// directory's to answer. Any other host or protocol, a request with no path, a path whose owner
// is no GitHub login or one the bundle holds no directory for, an unset LEGION_GH_OWNERS_DIR,
// and a hosts.yml with no token answer nothing, exit 1, naming why on stderr, and git, which every
// shared clone tells credential.interactive=false, then fails rather than prompts. `store` and
// `erase` change nothing: the daemon is the one writer of the gh files.
func runGitCredential(_ context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && helpRequested(args[0]) {
		fmt.Fprintln(stderr, gitCredentialUsage)
		return 0
	}
	if len(args) == 2 && helpRequested(args[1]) {
		if usage, ok := gitCredentialOperationUsage[args[0]]; ok {
			fmt.Fprintln(stderr, usage)
			return 0
		}
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
	ownersDir := os.Getenv("LEGION_GH_OWNERS_DIR")
	if ownersDir == "" {
		fmt.Fprintln(stderr, "legion git-credential: LEGION_GH_OWNERS_DIR is unset; answering nothing")
		return 1
	}
	if request["path"] == "" {
		fmt.Fprintln(stderr, "legion git-credential: git sent no path (credential.useHttpPath is off), so the request names no owner; answering nothing")
		return 1
	}
	path := strings.TrimPrefix(request["path"], "/")
	segment, rest, _ := strings.Cut(path, "/")
	owner := strings.ToLower(segment)
	if !ghconfig.IsOwner(owner) {
		fmt.Fprintf(stderr, "legion git-credential: the path %s names the owner %q, which is no GitHub login; answering nothing\n", request["path"], segment)
		return 1
	}
	repo, _, _ := strings.Cut(rest, "/")
	dir, err := repositoryGHConfigDir(ownersDir, owner, repo)
	if err != nil {
		fmt.Fprintf(stderr, "legion git-credential: %v; answering nothing\n", err)
		return 1
	}
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

// repositoryGHConfigDir is the gh directory the bundle under ownersDir answers for owner's
// repository repo: the repository's own, `<owner>/<repo>`, whose token reaches that one
// repository, where the bundle holds it, and the owner's own otherwise. It normalizes the name
// for a directory (ghRepositoryName), so a caller — the gh wrapper's target
// (gh_selectors.go) as much as a git credential request's path — may hand it as it was given
// it; the gh wrapper keys a repository by that same name as it reads a command, so two
// spellings of one repository are one repository, and normalizing it again here changes
// nothing for any name GitHub holds.
// A repo no bundle directory could be named for (ghconfig.IsDirName) is one
// the bundle holds none for, so it answers the owner's rather than refusing. An owner the bundle
// holds no directory for at all is refused (ownerGHConfigDir): it is no owner of this project.
func repositoryGHConfigDir(ownersDir, owner, repo string) (string, error) {
	if name := owner + "/" + ghRepositoryName(repo); repo != "" && ghconfig.IsDirName(name) {
		if dir := ghconfig.Dir(ownersDir, name); isGHConfigDir(dir) {
			return dir, nil
		}
	}
	return ownerGHConfigDir(ownersDir, owner)
}

// ghRepositoryName is a repository's name as the bundle directory holding its token is named for
// it: lowercased, and without the `.git` git's credential path and a clone URL carry, which gh
// trims as it reads a remote (ghrepo.FromURL) and which no GitHub repository's own name ends in.
// One trim is all a spelling of such a name needs, so a second call changes nothing; a name
// ending `.git.git` would lose one suffix a call, and GitHub holds no repository of that name.
func ghRepositoryName(repo string) string {
	return strings.TrimSuffix(strings.ToLower(repo), ".git")
}

// ownerGHConfigDir is owner's gh directory of the bundle under ownersDir, refused when the bundle
// holds none: owner is then no owner of this project.
func ownerGHConfigDir(ownersDir, owner string) (string, error) {
	dir := ghconfig.Dir(ownersDir, owner)
	if !isGHConfigDir(dir) {
		return "", fmt.Errorf("%q is not an owner of this project: %s holds no gh directory for it", owner, ownersDir)
	}
	return dir, nil
}

// isGHConfigDir says whether dir is a directory the bundle holds, and so one a gh or a git
// request may be answered from.
func isGHConfigDir(dir string) bool {
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}
