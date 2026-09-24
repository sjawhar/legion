package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
)

const provisioningTokenFileEnv = "LEGION_PROVISIONING_TOKEN_FILE"

// provisioningHelper is the one-shot credential: a git credential helper that answers `get` with
// the token. git asks it for https://github.com alone (the helper is
// credential.https://github.com.helper, set after a reset of every helper configured before it),
// so a remote rewritten to another scheme, host, or port gets nothing, and no askpass is asked in
// its place. In a pod the one process holding it, Fetch, reads no configuration a tree agent can
// write, so the only remote it asks for is the one its own argv names. On the tmux runtime the
// credentialed clone and fetch read the shared clone's configuration, where an http.proxy the tree
// wrote with http.sslVerify off still sees the token on its way to github.com: there the scope is
// defence, not a boundary (config.go).
const provisioningHelper = `#!/bin/sh
[ "$1" = get ] || exit 0
printf 'username=x-access-token\npassword=%s\n' "$(cat "$LEGION_PROVISIONING_TOKEN_FILE")"
`

// remote is how a clone or fetch reaches the repository: the environment it runs with, and the
// directory of the one-shot credential that environment names, if any, removed once it is done.
type remote struct {
	env []string
	dir string
}

// newProvisioningCredential ports the one-shot credential in workspace.ts:91-142. The Go daemon
// keeps the token in a 0600 file under parent, so the processes it is handed to receive only a file
// pointer and never a secret environment value. GIT_ASKPASS is set empty, which git reads as no
// askpass at all — neither core.askPass nor SSH_ASKPASS — and terminal prompts are off.
func newProvisioningCredential(parent, token string) (remote, error) {
	if token == "" {
		return remote{}, errors.New("workspace provisioning token is required")
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return remote{}, fmt.Errorf("create provisioning credential parent %s: %w", parent, err)
	}
	directory, err := os.MkdirTemp(parent, "provisioning-credential-")
	if err != nil {
		return remote{}, fmt.Errorf("create provisioning credential directory: %w", err)
	}
	cleanup := func(cause error) (remote, error) {
		if removeErr := os.RemoveAll(directory); removeErr != nil {
			return remote{}, fmt.Errorf("%w; remove provisioning credential directory: %v", cause, removeErr)
		}
		return remote{}, cause
	}
	tokenFile := filepath.Join(directory, "token")
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		return cleanup(fmt.Errorf("write provisioning token: %w", err))
	}
	helper := filepath.Join(directory, "helper")
	if err := os.WriteFile(helper, []byte(provisioningHelper), 0o700); err != nil {
		return cleanup(fmt.Errorf("write provisioning credential helper: %w", err))
	}
	if err := os.Chmod(helper, 0o700); err != nil {
		return cleanup(fmt.Errorf("chmod provisioning credential helper: %w", err))
	}
	return remote{
		dir: directory,
		env: []string{
			"GIT_ASKPASS=",
			"GIT_TERMINAL_PROMPT=0",
			provisioningTokenFileEnv + "=" + tokenFile,
			"GIT_CONFIG_COUNT=2",
			"GIT_CONFIG_KEY_0=credential.helper",
			"GIT_CONFIG_VALUE_0=",
			"GIT_CONFIG_KEY_1=credential.https://github.com.helper",
			"GIT_CONFIG_VALUE_1=!" + shellprefix.Literal(helper),
		},
	}, nil
}

func (r remote) remove() error {
	if r.dir == "" {
		return nil
	}
	if err := os.RemoveAll(r.dir); err != nil {
		return fmt.Errorf("remove provisioning credential directory: %w", err)
	}
	return nil
}

// isolatedGitConfig is the configuration Fetch's git reads besides its own: none. Not the system's,
// not the image user's; and git's `-c`, GIT_CONFIG_PARAMETERS, no process provisioning starts
// inherits (config.go).
var isolatedGitConfig = []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"}

// FetchRequest is a pod's first init container's: the repository, the provisioning token, the
// directory its one-shot credential goes under, and the feed directory.
type FetchRequest struct {
	Repo          string
	Token         string
	CredentialDir string
	Feed          string
}

// FeedRepository is where Fetch clones repository under a feed directory, and where Provision
// clones and fetches it from.
func FeedRepository(feed, repository string) (string, error) {
	owner, repo, err := splitRepository(repository)
	if err != nil {
		return "", err
	}
	if feed == "" {
		return "", errors.New("workspace feed directory is required")
	}
	return filepath.Join(feed, owner, repo+".git"), nil
}

// feedRemote reaches https://github.com/<repo>, the remote the shared clone's origin names, at the
// feed repository instead, over git's file transport alone: no credential, and no network.
func feedRemote(feed, repo string) remote {
	return remote{env: []string{
		"GIT_ALLOW_PROTOCOL=file",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=url." + feed + ".insteadOf",
		"GIT_CONFIG_VALUE_0=https://github.com/" + repo,
	}}
}

// Fetch clones the repository from GitHub, bare, into the feed, with the provisioning token and
// no configuration but the one-shot credential's and the pins: the process of a pod's first init
// container, the only one that holds the token, whose feed is its own and which mounts nothing a
// tree agent can write. Provision then clones and fetches the shared clone from the feed with no
// credential. It returns the feed repository.
func Fetch(ctx context.Context, run Runner, request FetchRequest) (string, error) {
	feed, err := FeedRepository(request.Feed, request.Repo)
	if err != nil {
		return "", err
	}
	if request.CredentialDir == "" {
		return "", errors.New("workspace credential directory is required")
	}
	credential, err := newProvisioningCredential(request.CredentialDir, request.Token)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = credential.remove()
	}()
	if err := os.MkdirAll(filepath.Dir(feed), 0o700); err != nil {
		return "", fmt.Errorf("create feed parent: %w", err)
	}
	clone := []string{"git", "clone", "--bare", "--quiet", "https://github.com/" + request.Repo, feed}
	if _, err := RunChecked(ctx, run, clone, merge(credential.env, isolatedGitConfig), ""); err != nil {
		return "", err
	}
	return feed, credential.remove()
}

// ensureRepoClone ports workspace.ts:144-196: clone into a temporary sibling, verify it contains
// .jj, then rename it into place. A killed clone can therefore never appear to be a final clone.
func ensureRepoClone(ctx context.Context, run Runner, cloneDir, remote string, remoteEnv []string) error {
	jjDir := filepath.Join(cloneDir, ".jj")
	if exists, err := pathExists(cloneDir); err != nil {
		return err
	} else if exists {
		if complete, err := pathExists(jjDir); err != nil {
			return err
		} else if complete {
			return nil
		}
		if err := os.RemoveAll(cloneDir); err != nil {
			return fmt.Errorf("remove incomplete clone %s: %w", cloneDir, err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(cloneDir), 0o700); err != nil {
		return fmt.Errorf("create clone parent: %w", err)
	}
	temporary, err := os.MkdirTemp(filepath.Dir(cloneDir), filepath.Base(cloneDir)+".clone-")
	if err != nil {
		return fmt.Errorf("create temporary clone directory: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(temporary)
	}()

	if _, err := RunChecked(ctx, run, []string{"jj", "git", "clone", remote, temporary}, remoteEnv, ""); err != nil {
		return err
	}
	complete, err := pathExists(filepath.Join(temporary, ".jj"))
	if err != nil {
		return err
	}
	if !complete {
		return fmt.Errorf("incomplete Jujutsu clone at %s: missing .jj", temporary)
	}
	if err := os.Rename(temporary, cloneDir); err != nil {
		if cloneCollision(err) {
			complete, statErr := pathExists(jjDir)
			if statErr == nil && complete {
				return nil
			}
		}
		return fmt.Errorf("rename cloned repository into place: %w", err)
	}
	return nil
}

func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func cloneCollision(err error) bool {
	return errors.Is(err, fs.ErrExist) || errors.Is(err, syscall.EEXIST) || errors.Is(err, syscall.ENOTEMPTY) || strings.Contains(err.Error(), "file exists")
}
