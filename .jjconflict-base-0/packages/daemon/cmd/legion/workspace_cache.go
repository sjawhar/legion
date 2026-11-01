package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/repocache"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const workspaceCacheUsage = "legion workspace-init cache --repo <owner>/<repo> --root <dir> [--listen :8418] [--refresh 10m]"

func runWorkspaceCache(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("workspace-init cache", "usage: "+workspaceCacheUsage, stderr)
	repo := flags.String("repo", "", "repository as <owner>/<name> (required)")
	root := flags.String("root", "", "the cache's own volume, where the bare clone lives (required)")
	listen := flags.String("listen", fmt.Sprintf(":%d", repocache.Port), "the address the cache serves /healthz and /tar on")
	refresh := flags.Duration("refresh", 10*time.Minute, "how long between fetches from GitHub")
	if code, ok := parseWorkspaceInitFlags(flags, args, stderr); !ok {
		return code
	}
	return workspaceInitExit(flags, workspaceCache(ctx, *repo, *root, *listen, *refresh, stdout), stderr)
}

// workspaceCache is a cache pod's one process (repocache.Serve): it reads --repo before anything
// else, then checks the rest of its input — the root, the refresh, the provisioning token, the
// bearer token its clients present (LEGION_REPOSITORY_CACHE_TOKEN_FILE), git on PATH — before it
// listens, runs git or touches its root, so a refusal leaves the volume as it was. It runs until
// its context ends, each log line on stdout.
func workspaceCache(ctx context.Context, repo, root, listen string, refresh time.Duration, stdout io.Writer) error {
	repository, err := ghrepo.Parse("--repo", repo)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(root) {
		return fmt.Errorf("--root must be an absolute path (got %q)", root)
	}
	if refresh <= 0 {
		return fmt.Errorf("--refresh must be positive (got %s)", refresh)
	}
	tokenFile, err := secretFileFromEnv(provisionTokenFileEnv)
	if err != nil {
		return err
	}
	bearerFile, err := secretFileFromEnv(repocache.CacheTokenFileEnv)
	if err != nil {
		return err
	}
	tools, err := provisioningTools("git")
	if err != nil {
		return err
	}
	return repocache.Serve(ctx, repocache.Options{
		Repo: repository, Root: root, TokenFile: tokenFile, CacheTokenFile: bearerFile, CredentialDir: os.TempDir(), Refresh: refresh, Listen: listen,
		Log: func(line string) { fmt.Fprintln(stdout, "workspace-init cache: "+line) },
		Run: workspace.NewRunner(workspace.CommandTimeout, tools),
	})
}
