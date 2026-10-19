package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// fakeGit is the git first on PATH. Its bare clone of github.com/acme/widgets — the fetch's —
// clones the local bare remote instead, whose file transport joins the https the runner allows,
// after appending the one-shot credential it was handed, the token file's path and what it held,
// to WINIT_CREDENTIAL_LOG; its fetch from github.com/acme/widgets into a bare repository (a feed
// that holds the repository, or one unpacked from the repository cache), and that fetch's read of
// GitHub's HEAD (`ls-remote --symref`), reach that remote in the same way. With
// WINIT_CLONE_FAILURES set (failClones), the clone first counts itself in
// that directory's `attempts`, and an attempt whose number names a file there fails as git does:
// it leaves a partial clone at its destination, writes the file to stderr with @TOKEN@ replaced by
// the token it was handed, and exits 128, git's fatal status. Every other invocation is the real
// git's.
const fakeGit = `#!/bin/sh
if [ "$1 $2 $3 $4" = "clone --bare --quiet https://github.com/acme/widgets" ]; then
	printf '%s %s\n' "$LEGION_PROVISIONING_TOKEN_FILE" "$(cat "$LEGION_PROVISIONING_TOKEN_FILE")" >> "$WINIT_CREDENTIAL_LOG"
	if [ -n "$WINIT_CLONE_FAILURES" ]; then
		attempt=$(( $(cat "$WINIT_CLONE_FAILURES/attempts" 2>/dev/null || echo 0) + 1 ))
		echo "$attempt" > "$WINIT_CLONE_FAILURES/attempts"
		if [ -f "$WINIT_CLONE_FAILURES/$attempt" ]; then
			mkdir -p "$5" && echo partial > "$5/partial"
			sed "s/@TOKEN@/$(cat "$LEGION_PROVISIONING_TOKEN_FILE")/g" "$WINIT_CLONE_FAILURES/$attempt" >&2
			exit 128
		fi
	fi
	shift 4
	GIT_ALLOW_PROTOCOL="$GIT_ALLOW_PROTOCOL:file" exec "$WINIT_REAL_GIT" clone --bare --quiet "$WINIT_REMOTE" "$@"
fi
if [ "$2 $3 $4 $5" = "fetch --prune --quiet https://github.com/acme/widgets" ]; then
	printf '%s %s\n' "$LEGION_PROVISIONING_TOKEN_FILE" "$(cat "$LEGION_PROVISIONING_TOKEN_FILE")" >> "$WINIT_CREDENTIAL_LOG"
	GIT_ALLOW_PROTOCOL="$GIT_ALLOW_PROTOCOL:file" exec "$WINIT_REAL_GIT" "$1" fetch --prune --quiet "$WINIT_REMOTE" "$6"
fi
if [ "$2 $3 $4 $5" = "ls-remote --symref https://github.com/acme/widgets HEAD" ]; then
	printf '%s %s\n' "$LEGION_PROVISIONING_TOKEN_FILE" "$(cat "$LEGION_PROVISIONING_TOKEN_FILE")" >> "$WINIT_CREDENTIAL_LOG"
	GIT_ALLOW_PROTOCOL="$GIT_ALLOW_PROTOCOL:file" exec "$WINIT_REAL_GIT" "$1" ls-remote --symref "$WINIT_REMOTE" HEAD
fi
exec "$WINIT_REAL_GIT" "$@"
`

// Every refusal `fetch` makes happens before it runs git or writes anything: the feed is not
// created, and no one-shot credential is left in its TMPDIR.
func TestWorkspaceInitFetchRefusesBeforeFetching(t *testing.T) {
	for _, tc := range []struct {
		name string
		args func(v *issueVolume) []string
		env  func(t *testing.T, v *issueVolume)
		code int
		says func(v *issueVolume) string
	}{
		{
			name: "a --repo that is not owner/name",
			args: func(v *issueVolume) []string { return []string{"fetch", "--repo", "acme", "--feed", v.feed} },
			code: 1,
			says: func(*issueVolume) string { return `--repo must be "owner/name" (got "acme")` },
		},
		{
			name: "a --repo with a .. segment",
			args: func(v *issueVolume) []string { return []string{"fetch", "--repo", "../x", "--feed", v.feed} },
			code: 1,
			says: func(*issueVolume) string { return `--repo "../x" has a ".." segment` },
		},
		{
			name: "a --repo holding whitespace",
			args: func(v *issueVolume) []string { return []string{"fetch", "--repo", "acme/wid gets", "--feed", v.feed} },
			code: 1,
			says: func(*issueVolume) string { return `--repo "acme/wid gets" holds whitespace` },
		},
		{
			// --repo is read first: the feed and the token are wrong too.
			name: "a bad --repo, before every other input",
			args: func(*issueVolume) []string { return []string{"fetch", "--repo", "acme/..", "--feed", "feed"} },
			env:  func(t *testing.T, _ *issueVolume) { unsetenv(t, "LEGION_PROVISION_TOKEN_FILE") },
			code: 1,
			says: func(*issueVolume) string { return `--repo "acme/.." has a ".." segment` },
		},
		{
			name: "a relative --feed",
			args: func(*issueVolume) []string { return []string{"fetch", "--repo", winitRepo, "--feed", "feed"} },
			code: 1,
			says: func(*issueVolume) string { return `--feed must be an absolute path (got "feed")` },
		},
		{
			name: "LEGION_PROVISION_TOKEN_FILE unset",
			env:  func(t *testing.T, _ *issueVolume) { unsetenv(t, "LEGION_PROVISION_TOKEN_FILE") },
			code: 1,
			says: func(*issueVolume) string { return "LEGION_PROVISION_TOKEN_FILE is not set" },
		},
		{
			name: "a provisioning token file that is absent",
			env: func(t *testing.T, v *issueVolume) {
				t.Setenv("LEGION_PROVISION_TOKEN_FILE", v.token+".absent")
			},
			code: 1,
			says: func(v *issueVolume) string {
				return "LEGION_PROVISION_TOKEN_FILE names " + v.token + ".absent, which could not be read"
			},
		},
		{
			name: "a provisioning token file that is blank",
			env: func(t *testing.T, v *issueVolume) {
				if err := os.WriteFile(v.token, []byte(" \n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			code: 1,
			says: func(v *issueVolume) string {
				return "LEGION_PROVISION_TOKEN_FILE names " + v.token + ", which is empty"
			},
		},
		{
			name: "no git on PATH",
			env:  func(t *testing.T, _ *issueVolume) { t.Setenv("PATH", t.TempDir()) },
			code: 1,
			says: func(*issueVolume) string { return "git is not on PATH" },
		},
		{
			name: "a --cache-url that is not an http URL",
			args: func(v *issueVolume) []string { return append(v.fetchArgs(), "--cache-url", "ftp://10.0.0.5:8418") },
			code: 1,
			says: func(*issueVolume) string { return `--cache-url must be an http:// URL (got "ftp://10.0.0.5:8418")` },
		},
		{
			name: "a LEGION_REPOSITORY_CACHE_URL that is not an http URL",
			env:  func(t *testing.T, _ *issueVolume) { t.Setenv("LEGION_REPOSITORY_CACHE_URL", "10.0.0.5:8418") },
			code: 1,
			says: func(*issueVolume) string { return `--cache-url must be an http:// URL (got "10.0.0.5:8418")` },
		},
		{
			name: "a --cache-url with no LEGION_REPOSITORY_CACHE_TOKEN_FILE",
			args: func(v *issueVolume) []string { return append(v.fetchArgs(), "--cache-url", "http://10.0.0.5:8418") },
			code: 1,
			says: func(*issueVolume) string {
				return "LEGION_REPOSITORY_CACHE_TOKEN_FILE is not set: a repository cache needs its token"
			},
		},
		{
			name: "a cache token file that is blank",
			args: func(v *issueVolume) []string { return append(v.fetchArgs(), "--cache-url", "http://10.0.0.5:8418") },
			env: func(t *testing.T, _ *issueVolume) {
				t.Setenv("LEGION_REPOSITORY_CACHE_TOKEN_FILE", writeCacheToken(t, "\n"))
			},
			code: 1,
			says: func(*issueVolume) string { return "LEGION_REPOSITORY_CACHE_TOKEN_FILE names " },
		},
		{
			name: "an unknown flag",
			args: func(v *issueVolume) []string { return append(v.fetchArgs(), "--root", v.root) },
			code: 2,
			says: func(*issueVolume) string { return "-root" },
		},
		{
			name: "a positional argument",
			args: func(v *issueVolume) []string { return append(v.fetchArgs(), "extra") },
			code: 2,
			says: func(*issueVolume) string { return `unexpected argument "extra"` },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newIssueVolume(t)
			v.setenv(t)
			t.Setenv("LEGION_PROVISION_TOKEN_FILE", v.token)
			if tc.env != nil {
				tc.env(t, v)
			}
			args := v.fetchArgs()
			if tc.args != nil {
				args = tc.args(v)
			}
			code, stdout, stderr := runWorkspaceInitHere(args)
			if code != tc.code || !strings.Contains(stderr, tc.says(v)) {
				t.Fatalf("exit %d, stderr %q; want %d naming %q", code, stderr, tc.code, tc.says(v))
			}
			if stdout != "" {
				t.Fatalf("stdout %q, want nothing", stdout)
			}
			if _, err := os.Stat(v.feed); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the feed was created (%v)", err)
			}
			if entries, err := os.ReadDir(v.tmp); err != nil || len(entries) != 0 {
				t.Fatalf("TMPDIR holds %v (%v), want no credential left behind", entries, err)
			}
			if _, err := os.Stat(v.credentialLog); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("git ran a clone before the refusal (%v)", err)
			}
		})
	}
}

// `fetch` clones the repository bare into the feed with the provisioning token, which its git is
// handed as a one-shot credential on the container's own filesystem (its TMPDIR) and which is gone
// once it returns; the feed never holds it.
func TestWorkspaceInitFetchFillsTheFeed(t *testing.T) {
	v := newIssueVolume(t).withRemote(t, "main")
	v.setenv(t)
	t.Setenv("LEGION_PROVISION_TOKEN_FILE", v.token)

	code, stdout, stderr := runWorkspaceInitHere(v.fetchArgs())
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	feed := filepath.Join(v.feed, "acme", "widgets.git")
	if !noCacheLine.MatchString(stdout) {
		t.Fatalf("stdout %q, want the clone from GitHub with no repository cache", stdout)
	}
	if printed, held := printedBytes(t, noCacheLine.FindStringSubmatch(stdout)[1]), objectBytes(t, feed); printed != held || held == 0 {
		t.Fatalf("the clone printed %d bytes from GitHub, want the %d the feed's objects hold", printed, held)
	}
	if main := v.git(t, "--git-dir="+feed, "log", "-1", "--format=%s", "refs/heads/main"); main != "seed" {
		t.Fatalf("the feed's main is %q, want the remote's", main)
	}
	credential, err := os.ReadFile(v.credentialLog)
	if err != nil {
		t.Fatalf("the clone recorded no credential: %v", err)
	}
	tokenFile, held, _ := strings.Cut(strings.TrimSpace(string(credential)), " ")
	if held != "ghs_test" || !strings.HasPrefix(tokenFile, v.tmp+string(filepath.Separator)) {
		t.Fatalf("the clone was handed %s holding %q, want the provisioning token under TMPDIR %s", tokenFile, held, v.tmp)
	}
	if entries, err := os.ReadDir(v.tmp); err != nil || len(entries) != 0 {
		t.Fatalf("TMPDIR holds %v (%v) after fetch returned, want its credential gone", entries, err)
	}
	holdsNoToken(t, v.feed, "after fetch")
}

// The two transient failures a production fetch met from GitHub, as git prints them: an HTTP/2
// stream GitHub cancelled mid-pack, and a 408 on the pack request.
const (
	curl92Failure = "error: RPC failed; curl 92 HTTP/2 stream 7 was not closed cleanly: CANCEL (err 8)\n" +
		"fatal: early EOF\n" +
		"fatal: fetch-pack: invalid index-pack output\n"
	http408Failure = "error: RPC failed; HTTP 408 curl 22 The requested URL returned error: 408\n" +
		"fatal: expected 'packfile'\n"
)

// failClones has the volume's git fail its first clones of github.com/acme/widgets, one per
// entry of stderr, in order (fakeGit), and returns the file that counts every clone attempt.
func (v *issueVolume) failClones(t *testing.T, stderr ...string) string {
	t.Helper()
	dir := t.TempDir()
	for attempt, text := range stderr {
		if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(attempt+1)), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	v.env["WINIT_CLONE_FAILURES"] = dir
	return filepath.Join(dir, "attempts")
}

// cloneAttempts is how many clones of github.com/acme/widgets the volume's git saw.
func cloneAttempts(t *testing.T, attempts string) int {
	t.Helper()
	body, err := os.ReadFile(attempts)
	if err != nil {
		t.Fatalf("the fetch ran no clone: %v", err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	return count
}

// instantClock is the fetch's clock in a test: each wait passes at once, recorded, and moves the
// clock on by its length.
type instantClock struct {
	now   time.Time
	waits []time.Duration
}

func (c *instantClock) Now() time.Time { return c.now }

func (c *instantClock) Sleep(ctx context.Context, d time.Duration) error {
	c.waits = append(c.waits, d)
	c.now = c.now.Add(d)
	return ctx.Err()
}

// useInstantClock makes an instantClock the fetch's clock until the test ends.
func useInstantClock(t *testing.T) *instantClock {
	t.Helper()
	clock := &instantClock{now: time.Date(2026, 10, 13, 17, 49, 23, 0, time.UTC)}
	saved := fetchClock
	fetchClock = clock
	t.Cleanup(func() { fetchClock = saved })
	return clock
}

// A clone that fails transiently is run again after a backoff wait, each failure logged on stdout
// as one line, until one succeeds and fills the feed. Real git removes its own destination when a
// clone fails; this git leaves a partial clone there anyway, so the test holds the fetch to removing
// whatever a failed attempt left before it clones again.
func TestWorkspaceInitFetchRetriesATransientCloneFailure(t *testing.T) {
	v := newIssueVolume(t).withRemote(t, "main")
	attempts := v.failClones(t, curl92Failure, http408Failure)
	clock := useInstantClock(t)
	v.setenv(t)
	t.Setenv("LEGION_PROVISION_TOKEN_FILE", v.token)

	code, stdout, stderr := runWorkspaceInitHere(v.fetchArgs())
	if code != 0 {
		t.Fatalf("exit %d, stderr %q, stdout %q", code, stderr, stdout)
	}
	feed := filepath.Join(v.feed, "acme", "widgets.git")
	want := "workspace-init fetch: clone attempt 1 failed; retrying in 5s: error: RPC failed; curl 92 HTTP/2 stream 7 was not closed cleanly: CANCEL (err 8) | fatal: early EOF | fatal: fetch-pack: invalid index-pack output\n" +
		"workspace-init fetch: clone attempt 2 failed; retrying in 15s: error: RPC failed; HTTP 408 curl 22 The requested URL returned error: 408 | fatal: expected 'packfile'\n"
	rest, retried := strings.CutPrefix(stdout, want)
	if !retried || !noCacheLine.MatchString(rest) {
		t.Fatalf("stdout %q, want %q then the clone from GitHub", stdout, want)
	}
	if stderr != "" {
		t.Fatalf("stderr %q, want nothing", stderr)
	}
	if got := cloneAttempts(t, attempts); got != 3 {
		t.Fatalf("git cloned %d times, want 3", got)
	}
	if waits := clock.waits; !slices.Equal(waits, []time.Duration{5 * time.Second, 15 * time.Second}) {
		t.Fatalf("the fetch waited %v, want 5s then 15s", waits)
	}
	if main := v.git(t, "--git-dir="+feed, "log", "-1", "--format=%s", "refs/heads/main"); main != "seed" {
		t.Fatalf("the feed's main is %q, want the remote's", main)
	}
	if _, err := os.Stat(filepath.Join(feed, "partial")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the feed still holds a failed attempt's partial clone (%v)", err)
	}
	if entries, err := os.ReadDir(v.tmp); err != nil || len(entries) != 0 {
		t.Fatalf("TMPDIR holds %v (%v) after fetch returned, want its credential gone", entries, err)
	}
}

// A clone that keeps failing transiently is retried four times, one per backoff wait, and the
// fifth failure fails the fetch: the four retries on stdout, the failure and the limit on stderr.
func TestWorkspaceInitFetchGivesUpAfterItsLastRetry(t *testing.T) {
	v := newIssueVolume(t).withRemote(t, "main")
	attempts := v.failClones(t, http408Failure, http408Failure, http408Failure, http408Failure, http408Failure, http408Failure)
	clock := useInstantClock(t)
	v.setenv(t)
	t.Setenv("LEGION_PROVISION_TOKEN_FILE", v.token)

	code, stdout, stderr := runWorkspaceInitHere(v.fetchArgs())
	if says := "clone attempt 5 failed, and the fetch retries a clone at most 4 times"; code != 1 || !strings.Contains(stderr, says) {
		t.Fatalf("exit %d, stderr %q; want 1 naming %q", code, stderr, says)
	}
	if retries := strings.Count(stdout, "workspace-init fetch: clone attempt "); retries != 4 {
		t.Fatalf("stdout %q logs %d retries, want 4", stdout, retries)
	}
	if got := cloneAttempts(t, attempts); got != 5 {
		t.Fatalf("git cloned %d times, want 5", got)
	}
	if waits := clock.waits; !slices.Equal(waits, []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second, time.Minute}) {
		t.Fatalf("the fetch waited %v, want 5s, 15s, 30s, 1m", waits)
	}
}

// A clone GitHub refuses for good fails on its first attempt, with no wait and no retry, naming
// git's own message on stderr and logging nothing on stdout. Which failures are permanent is
// workspace.Fetch's to tell; this is what the command does with one.
func TestWorkspaceInitFetchFailsAPermanentCloneFailureAtOnce(t *testing.T) {
	v := newIssueVolume(t).withRemote(t, "main")
	notFound := "remote: Repository not found.\nfatal: repository 'https://github.com/acme/widgets/' not found\n"
	attempts := v.failClones(t, notFound, notFound)
	clock := useInstantClock(t)
	v.setenv(t)
	t.Setenv("LEGION_PROVISION_TOKEN_FILE", v.token)

	code, stdout, stderr := runWorkspaceInitHere(v.fetchArgs())
	if says := "fatal: repository 'https://github.com/acme/widgets/' not found"; code != 1 || !strings.Contains(stderr, says) {
		t.Fatalf("exit %d, stderr %q; want 1 naming %q", code, stderr, says)
	}
	if stdout != "" {
		t.Fatalf("stdout %q, want nothing", stdout)
	}
	if got := cloneAttempts(t, attempts); got != 1 {
		t.Fatalf("git cloned %d times, want 1", got)
	}
	if waits := clock.waits; len(waits) != 0 {
		t.Fatalf("the fetch waited %v, want no wait", waits)
	}
}

// Neither a retried attempt's log line nor the final failure carries the provisioning token, even
// where git echoes it: each says *** in its place.
func TestWorkspaceInitFetchLogsNoToken(t *testing.T) {
	v := newIssueVolume(t).withRemote(t, "main")
	v.failClones(t,
		"fatal: unable to access 'https://x-access-token:@TOKEN@@github.com/acme/widgets/': Failed to connect to github.com port 443 after 127010 ms: Couldn't connect to server\n",
		"remote: Invalid username or token @TOKEN@.\nfatal: Authentication failed for 'https://x-access-token:@TOKEN@@github.com/acme/widgets/'\n",
	)
	useInstantClock(t)
	v.setenv(t)
	t.Setenv("LEGION_PROVISION_TOKEN_FILE", v.token)

	code, stdout, stderr := runWorkspaceInitHere(v.fetchArgs())
	if code != 1 {
		t.Fatalf("exit %d, stderr %q; want 1", code, stderr)
	}
	if strings.Contains(stdout+stderr, "ghs_test") {
		t.Fatalf("the fetch logged the provisioning token:\nstdout %q\nstderr %q", stdout, stderr)
	}
	if want := "workspace-init fetch: clone attempt 1 failed; retrying in 5s: fatal: unable to access 'https://x-access-token:***@github.com/acme/widgets/': Failed to connect to github.com port 443 after 127010 ms: Couldn't connect to server\n"; stdout != want {
		t.Fatalf("stdout %q, want %q", stdout, want)
	}
	for _, says := range []string{"remote: Invalid username or token ***.", "fatal: Authentication failed for 'https://x-access-token:***@github.com/acme/widgets/'"} {
		if !strings.Contains(stderr, says) {
			t.Fatalf("stderr %q, want it to name %q", stderr, says)
		}
	}
}

// noCacheLine is the one line `fetch` prints when no repository cache is named: the clone from
// GitHub, with its bytes.
var noCacheLine = regexp.MustCompile(`^workspace-init fetch: no repository cache named: cloned acme/widgets from GitHub \(([0-9,]+) bytes\)\n$`)

// printedBytes is a byte count as `fetch` prints it, digits grouped by commas.
func printedBytes(t *testing.T, printed string) int64 {
	t.Helper()
	if !regexp.MustCompile(`^[0-9]{1,3}(,[0-9]{3})*$`).MatchString(printed) {
		t.Fatalf("%q is not a count grouped by commas", printed)
	}
	n, err := strconv.ParseInt(strings.ReplaceAll(printed, ",", ""), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// objectBytes is how many bytes the regular files under the bare repository's objects hold.
func objectBytes(t *testing.T, gitDir string) int64 {
	t.Helper()
	var size int64
	err := filepath.WalkDir(filepath.Join(gitDir, "objects"), func(_ string, entry os.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return err
		}
		info, err := entry.Info()
		if err == nil {
			size += info.Size()
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return size
}

// advanceRemote puts one more commit on the volume's remote's main and returns its id.
func (v *issueVolume) advanceRemote(t *testing.T) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	for _, argv := range [][]string{
		{"clone", "--quiet", v.env["WINIT_REMOTE"], work},
		{"-C", work, "-c", "user.name=Legion test", "-c", "user.email=legion-test@example.invalid", "commit", "--quiet", "--allow-empty", "-m", "advanced"},
		{"-C", work, "push", "--quiet", "origin", "HEAD:main"},
	} {
		if output, err := exec.Command(v.env["WINIT_REAL_GIT"], argv...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(argv, " "), err, output)
		}
	}
	return v.git(t, "--git-dir="+v.env["WINIT_REMOTE"], "rev-parse", "main")
}

// cacheTarServer serves, as the repository cache's GET /healthz and GET /tar, a ready cache and the
// history and refs of a bare clone of the volume's remote as it is now, and returns its address and
// the tar's length. The clone runs over git's transport, so its history is one pack, as the cache's
// is.
func (v *issueVolume) cacheTarServer(t *testing.T) (string, int) {
	t.Helper()
	cache := filepath.Join(t.TempDir(), "widgets.git")
	if output, err := exec.Command(v.env["WINIT_REAL_GIT"], "clone", "--bare", "--quiet", "--no-local", v.env["WINIT_REMOTE"], cache).CombinedOutput(); err != nil {
		t.Fatalf("clone the cache: %v\n%s", err, output)
	}
	var stream bytes.Buffer
	archive := tar.NewWriter(&stream)
	err := filepath.WalkDir(cache, func(path string, entry os.DirEntry, err error) error {
		if err != nil || path == cache {
			return err
		}
		relative, err := filepath.Rel(cache, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(relative)
		if !workspace.FeedEntry(name, entry.IsDir()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = name
		if entry.IsDir() {
			header.Name += "/"
			return archive.WriteHeader(header)
		}
		body, err := os.ReadFile(path)
		if err == nil {
			err = archive.WriteHeader(header)
		}
		if err == nil {
			_, err = archive.Write(body)
		}
		return err
	})
	if err == nil {
		err = archive.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	body := stream.Bytes()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(workspace.CacheRepositoryHeader, "acme/widgets")
		switch r.URL.Path {
		case "/healthz":
			_, _ = w.Write([]byte(`{"ready":true,"repo":"acme/widgets","bytes":1}`))
			return
		case "/tar":
			if r.Header.Get("Authorization") != "Bearer "+cmdCacheToken {
				w.Header().Del(workspace.CacheRepositoryHeader)
				http.Error(w, "the repository cache's token is required", http.StatusUnauthorized)
				return
			}
		default:
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return server.URL, len(body)
}

// With a ready repository cache — LEGION_REPOSITORY_CACHE_URL and its token's file, which the
// runtime sets on every issue pod's fetch container — `fetch` unpacks the cache's tar into the feed
// and fetches from GitHub, with the provisioning token, only what the remote gained since: one line
// says both, with each source's bytes. A relaunch, whose feed on the issue volume holds the
// repository already, fetches the delta alone and says so.
func TestWorkspaceInitFetchStreamsTheRepositoryCache(t *testing.T) {
	v := newIssueVolume(t).withRemote(t, "main")
	cache, length := v.cacheTarServer(t)
	head := v.advanceRemote(t)
	v.setenv(t)
	t.Setenv("LEGION_PROVISION_TOKEN_FILE", v.token)
	t.Setenv("LEGION_REPOSITORY_CACHE_URL", cache)
	t.Setenv("LEGION_REPOSITORY_CACHE_TOKEN_FILE", writeCacheToken(t, cmdCacheToken))

	code, stdout, stderr := runWorkspaceInitHere(v.fetchArgs())
	if code != 0 {
		t.Fatalf("exit %d, stderr %q, stdout %q", code, stderr, stdout)
	}
	line := regexp.MustCompile(`^workspace-init fetch: ([0-9,]+) bytes from the repository cache ` + regexp.QuoteMeta(cache) + ` in [0-9.]+(?:µs|ms|s), its history verified in [0-9.]+(?:µs|ms|s); ([0-9,]+) bytes from GitHub for the delta \(1 ref\)\n$`)
	match := line.FindStringSubmatch(stdout)
	if match == nil {
		t.Fatalf("stdout %q, want the cache's stream and GitHub's delta", stdout)
	}
	if printed := printedBytes(t, match[1]); printed != int64(length) {
		t.Fatalf("the line counts %d bytes from the cache, want the tar's %d", printed, length)
	}
	if delta := printedBytes(t, match[2]); delta <= 0 || delta >= 64<<10 {
		t.Fatalf("the line counts %d bytes from GitHub, want the one commit's few", delta)
	}
	feed := filepath.Join(v.feed, "acme", "widgets.git")
	if main := v.git(t, "--git-dir="+feed, "rev-parse", "refs/heads/main"); main != head {
		t.Fatalf("the feed's main is %s, want the remote's %s", main, head)
	}
	if credential := logLines(t, v.credentialLog); len(credential) != 2 || !strings.HasSuffix(credential[0], " ghs_test") || !strings.HasSuffix(credential[1], " ghs_test") {
		t.Fatalf("GitHub was reached with %q, want the delta's fetch and its read of GitHub's HEAD, each with the provisioning token", credential)
	}
	holdsNoToken(t, v.feed, "after fetch")

	code, stdout, stderr = runWorkspaceInitHere(v.fetchArgs())
	if code != 0 {
		t.Fatalf("relaunch: exit %d, stderr %q", code, stderr)
	}
	if !regexp.MustCompile(`^workspace-init fetch: the feed holds acme/widgets already: ([0-9,]+) bytes from GitHub for the delta \(0 refs\)\n$`).MatchString(stdout) {
		t.Fatalf("relaunch stdout %q, want the delta alone", stdout)
	}
}

// A cache that never becomes ready fails the fetch, exit 1, naming the cache and its last answer,
// after one waiting line per change of that answer: nothing is cloned from GitHub instead.
func TestWorkspaceInitFetchFailsWhenTheCacheIsNeverReady(t *testing.T) {
	v := newIssueVolume(t).withRemote(t, "main")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(workspace.CacheRepositoryHeader, "acme/widgets")
		http.Error(w, "the repository cache holds no clone yet", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	v.setenv(t)
	t.Setenv("LEGION_PROVISION_TOKEN_FILE", v.token)
	useInstantClock(t)

	t.Setenv("LEGION_REPOSITORY_CACHE_TOKEN_FILE", writeCacheToken(t, cmdCacheToken))
	code, stdout, stderr := runWorkspaceInitHere(append(v.fetchArgs(), "--cache-url", server.URL))
	if code != 1 || !strings.Contains(stderr, "the repository cache "+server.URL+" was not ready within "+workspace.CacheWaitTimeout.String()+": GET "+server.URL+"/healthz answered 503 Service Unavailable") {
		t.Fatalf("exit %d, stderr %q; want 1 naming the cache and its answer", code, stderr)
	}
	if !strings.HasPrefix(stdout, "workspace-init fetch: waiting for the repository cache "+server.URL+" to be ready: GET "+server.URL+"/healthz answered 503 Service Unavailable\n") {
		t.Fatalf("stdout %q, want the wait logged", stdout)
	}
	if _, err := os.Stat(v.credentialLog); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("GitHub was reached while the cache was not ready (%v)", err)
	}
}
