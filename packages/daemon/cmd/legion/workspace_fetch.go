package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/repocache"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const workspaceFetchUsage = "legion workspace-init fetch --repo <owner>/<repo> --feed <dir> [--cache-url http://<cache>:8418]"

// cacheURLEnv is where the runtime names the repository's cache to the fetch container, beside its
// token's file (LEGION_REPOSITORY_CACHE_TOKEN_FILE); --cache-url defaults to it.
const cacheURLEnv = "LEGION_REPOSITORY_CACHE_URL"

// fetchClock is the time the fetch's clone retries read and wait on; a test replaces it.
var fetchClock workspace.Clock = workspace.WallClock{}

// workspaceFetch fills the feed, the container's own directory on the issue's volume: from the
// repository cache when one is named, presenting the token LEGION_REPOSITORY_CACHE_TOKEN_FILE
// holds, then from GitHub for what it lacks; or from GitHub alone. The provisioning token is read
// from LEGION_PROVISION_TOKEN_FILE again before every transfer from GitHub, as the file stands then
// (the daemon keeps the Secret it is projected from fresh while the fetch runs), so a long wait on
// the cache never leaves the delta an expired token. It reads --repo before anything else, resolves
// git alone, and refuses before it ever touches the feed or its own TMPDIR, where the one-shot
// credential goes — every check (--repo, --feed, --cache-url and its token, a readable provisioning
// token, git on PATH) runs first, so a refusal here leaves neither behind. A transfer that fails
// transiently is run again (workspace.Fetch), each retried failure one line on stdout, and the last
// line says where the feed came from and the bytes each source brought.
func workspaceFetch(ctx context.Context, repo, feed, cacheURL string, stdout io.Writer) error {
	repository, err := ghrepo.Parse("--repo", repo)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(feed) {
		return fmt.Errorf("--feed must be an absolute path (got %q)", feed)
	}
	cacheToken := ""
	if cacheURL != "" {
		if parsed, err := url.Parse(cacheURL); err != nil || parsed.Scheme != "http" || parsed.Host == "" {
			return fmt.Errorf("--cache-url must be an http:// URL (got %q)", cacheURL)
		}
		tokenFile, set := os.LookupEnv(repocache.CacheTokenFileEnv)
		if !set {
			return errors.New(repocache.CacheTokenFileEnv + " is not set: a repository cache needs its token")
		}
		if cacheToken, err = config.ReadSecretPointer(repocache.CacheTokenFileEnv, tokenFile); err != nil {
			return err
		}
	}
	tokenFile, err := secretFileFromEnv(provisionTokenFileEnv)
	if err != nil {
		return err
	}
	token := func() (string, error) { return config.ReadSecretPointer(provisionTokenFileEnv, tokenFile) }
	tools, err := provisioningTools("git")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(feed, 0o700); err != nil {
		return fmt.Errorf("create the feed directory %s: %w", feed, err)
	}
	report, err := workspace.Fetch(ctx, workspace.NewRunner(workspace.CommandTimeout, tools), workspace.FetchRequest{
		Repo: repository, Token: token, CredentialDir: os.TempDir(), Feed: feed,
		Log: func(line string) { fmt.Fprintln(stdout, "workspace-init fetch: "+line) }, Clock: fetchClock, CacheURL: cacheURL, CacheToken: cacheToken,
	})
	if err != nil {
		return err
	}
	switch {
	case report.CacheBytes > 0:
		fmt.Fprintf(stdout, "workspace-init fetch: %s bytes from the repository cache %s in %s, its history verified in %s; %s bytes from GitHub for the delta (%s)\n",
			groupedBytes(report.CacheBytes), cacheURL, report.CacheDuration.Round(time.Millisecond), report.VerifyDuration.Round(time.Millisecond), groupedBytes(report.GitHubBytes), refCount(report.Refs))
	case report.Cloned:
		fmt.Fprintf(stdout, "workspace-init fetch: no repository cache named: cloned %s from GitHub (%s bytes)\n", repository, groupedBytes(report.GitHubBytes))
	default:
		fmt.Fprintf(stdout, "workspace-init fetch: the feed holds %s already: %s bytes from GitHub for the delta (%s)\n", repository, groupedBytes(report.GitHubBytes), refCount(report.Refs))
	}
	return nil
}

// groupedBytes is n with its digits grouped in threes by commas: 6,612,345,678.
func groupedBytes(n int64) string {
	digits := strconv.FormatInt(n, 10)
	grouped := make([]byte, 0, len(digits)+len(digits)/3)
	for i := range len(digits) {
		if i > 0 && (len(digits)-i)%3 == 0 && digits[i-1] != '-' {
			grouped = append(grouped, ',')
		}
		grouped = append(grouped, digits[i])
	}
	return string(grouped)
}

// refCount is how many refs a transfer changed, as a count of refs.
func refCount(n int) string {
	if n == 1 {
		return "1 ref"
	}
	return strconv.Itoa(n) + " refs"
}
