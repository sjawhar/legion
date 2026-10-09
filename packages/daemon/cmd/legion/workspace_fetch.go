package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const workspaceFetchUsage = "legion workspace-init fetch --repo <owner>/<repo> --feed <dir>"

// workspaceFetch reads the provisioning token and clones the repository from GitHub into the
// feed, the container's own volume, with no git configuration but its own. It reads --repo
// before anything else, resolves git alone, and refuses before it ever touches the feed or its
// own TMPDIR, where the one-shot credential goes — every check (--repo, --feed, the token, git on
// PATH) runs first, so a refusal here leaves neither behind. Only once every check has passed
// does it write fetchStartedFile into the feed, just before the clone starts:
// `workspace-init provision`'s own evidence, later, of when this pod's own fetch began, whatever
// the clone itself then takes (dispatch://LEGION-583, the removableWorkspacesEnv doc comment in
// workspace_init.go).
func workspaceFetch(ctx context.Context, repo, feed string, stdout io.Writer) error {
	repository, err := ghrepo.Parse("--repo", repo)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(feed) {
		return fmt.Errorf("--feed must be an absolute path (got %q)", feed)
	}
	tokenFile, set := os.LookupEnv(provisionTokenFileEnv)
	if !set {
		return errors.New(provisionTokenFileEnv + " is not set")
	}
	token, err := config.ReadSecretPointer(provisionTokenFileEnv, tokenFile)
	if err != nil {
		return err
	}
	tools, err := provisioningTools("git")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(feed, 0o700); err != nil {
		return fmt.Errorf("create the feed directory %s: %w", feed, err)
	}
	if err := os.WriteFile(filepath.Join(feed, fetchStartedFile), []byte(time.Now().Format(time.RFC3339)), 0o600); err != nil {
		return fmt.Errorf("record this pod's fetch start: %w", err)
	}
	fed, err := workspace.Fetch(ctx, workspace.NewRunner(workspace.CommandTimeout, tools), workspace.FetchRequest{
		Repo: repository, Token: token, CredentialDir: os.TempDir(), Feed: feed,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "workspace-init fetch: %s into %s\n", workspace.GitHubURL(repository), fed)
	return nil
}
