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
)

const (
	provisioningTokenFileEnv = "LEGION_PROVISIONING_TOKEN_FILE"
	provisioningRepoEnv      = "LEGION_PROVISIONING_REPO"
)

// provisioningAskpass answers exactly the prompts git writes for https://github.com: bare, or with
// the provisioned repository's path, which git adds under credential.useHttpPath (an operator's
// global git configuration can set it). A remote the tree rewrote to another host — the clone's
// remote URL or a url.<base>.insteadOf — gets a prompt naming that host, which the askpass
// refuses, so the token goes to github.com alone. Inside double quotes the expanded repository is
// matched literally.
const provisioningAskpass = `#!/bin/sh
case "$1" in
  "Username for 'https://github.com': " | "Username for 'https://github.com/$LEGION_PROVISIONING_REPO': ")
    printf '%s\n' x-access-token ;;
  "Password for 'https://x-access-token@github.com': " | "Password for 'https://x-access-token@github.com/$LEGION_PROVISIONING_REPO': ")
    cat "$LEGION_PROVISIONING_TOKEN_FILE" ;;
  *) exit 1 ;;
esac
`

type provisioningCredential struct {
	dir string
	env []string
}

// newProvisioningCredential ports the one-shot askpass credential in workspace.ts:91-142, for
// repo (owner/repository). The Go daemon keeps the token in a 0600 file under parent, so clone and
// fetch receive only a file pointer and never a secret environment value.
func newProvisioningCredential(parent, repo, token string) (provisioningCredential, error) {
	if token == "" {
		return provisioningCredential{}, errors.New("workspace provisioning token is required")
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return provisioningCredential{}, fmt.Errorf("create provisioning credential parent %s: %w", parent, err)
	}
	directory, err := os.MkdirTemp(parent, "provisioning-credential-")
	if err != nil {
		return provisioningCredential{}, fmt.Errorf("create provisioning credential directory: %w", err)
	}
	cleanup := func(cause error) (provisioningCredential, error) {
		if removeErr := os.RemoveAll(directory); removeErr != nil {
			return provisioningCredential{}, fmt.Errorf("%w; remove provisioning credential directory: %v", cause, removeErr)
		}
		return provisioningCredential{}, cause
	}
	tokenFile := filepath.Join(directory, "token")
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		return cleanup(fmt.Errorf("write provisioning token: %w", err))
	}
	askpass := filepath.Join(directory, "askpass")
	if err := os.WriteFile(askpass, []byte(provisioningAskpass), 0o700); err != nil {
		return cleanup(fmt.Errorf("write provisioning askpass: %w", err))
	}
	if err := os.Chmod(askpass, 0o700); err != nil {
		return cleanup(fmt.Errorf("chmod provisioning askpass: %w", err))
	}
	return provisioningCredential{
		dir: directory,
		env: []string{
			"GIT_ASKPASS=" + askpass,
			"GIT_TERMINAL_PROMPT=0",
			provisioningTokenFileEnv + "=" + tokenFile,
			provisioningRepoEnv + "=" + repo,
			"GIT_CONFIG_COUNT=2",
			"GIT_CONFIG_KEY_0=credential.helper",
			"GIT_CONFIG_VALUE_0=",
			"GIT_CONFIG_KEY_1=credential.interactive",
			"GIT_CONFIG_VALUE_1=true",
		},
	}, nil
}

func (credential provisioningCredential) remove() error {
	if credential.dir == "" {
		return nil
	}
	if err := os.RemoveAll(credential.dir); err != nil {
		return fmt.Errorf("remove provisioning credential directory: %w", err)
	}
	return nil
}

// ensureRepoClone ports workspace.ts:144-196: clone into a temporary sibling, verify it contains
// .jj, then rename it into place. A killed clone can therefore never appear to be a final clone.
func ensureRepoClone(ctx context.Context, run Runner, cloneDir, remote string, credentialEnv []string) error {
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

	if _, err := RunChecked(ctx, run, []string{"jj", "git", "clone", remote, temporary}, credentialEnv, ""); err != nil {
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
