package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeGit is the git first on PATH. Its bare clone of github.com/acme/widgets — the fetch's —
// clones the local bare remote instead, whose file transport joins the https the runner allows,
// after appending the one-shot credential it was handed, the token file's path and what it held,
// to WINIT_CREDENTIAL_LOG. With WINIT_CLONE_FAILURES set (failClones), the clone first counts
// itself in that directory's `attempts`, and an attempt whose number names a file there fails as
// git does: it leaves a partial clone at its destination, writes the file to stderr with @TOKEN@
// replaced by the token it was handed, and exits 128, git's fatal status. Every other invocation
// is the real git's.
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
	if want := "workspace-init fetch: https://github.com/acme/widgets into " + feed + "\n"; stdout != want {
		t.Fatalf("stdout %q, want %q", stdout, want)
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
		"workspace-init fetch: clone attempt 2 failed; retrying in 15s: error: RPC failed; HTTP 408 curl 22 The requested URL returned error: 408 | fatal: expected 'packfile'\n" +
		"workspace-init fetch: https://github.com/acme/widgets into " + feed + "\n"
	if stdout != want {
		t.Fatalf("stdout %q, want %q", stdout, want)
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
