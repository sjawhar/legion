package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const workspaceFetchUsage = "legion workspace-init fetch --repo <owner>/<repo> --feed <dir>"

// fetchClock is the time the fetch's clone retries read and wait on; a test replaces it.
var fetchClock workspace.Clock = workspace.WallClock{}

// workspaceFetch reads the provisioning token and clones the repository from GitHub into the
// feed, the container's own volume, with no git configuration but its own. It reads --repo
// before anything else, resolves git alone, and refuses before it ever touches the feed or its
// own TMPDIR, where the one-shot credential goes — every check (--repo, --feed, the token, git on
// PATH) runs first, so a refusal here leaves neither behind. A clone that fails transiently is
// cloned again (workspace.Fetch), each retried failure one line on stdout.
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
	fed, err := workspace.Fetch(ctx, workspace.NewRunner(workspace.CommandTimeout, tools), workspace.FetchRequest{
		Repo: repository, Token: token, CredentialDir: os.TempDir(), Feed: feed,
		Log: func(line string) { fmt.Fprintln(stdout, "workspace-init fetch: "+line) }, Clock: fetchClock,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "workspace-init fetch: %s into %s\n", workspace.GitHubURL(repository), fed)
	return nil
}
