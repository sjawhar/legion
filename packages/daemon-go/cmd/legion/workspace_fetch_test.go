package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGit is the git first on PATH. Its bare clone of github.com/acme/widgets — the fetch's —
// clones the local bare remote instead, whose file transport joins the https the runner allows,
// after appending the one-shot credential it was handed, the token file's path and what it held,
// to WINIT_CREDENTIAL_LOG. Every other invocation is the real git's.
const fakeGit = `#!/bin/sh
if [ "$1 $2 $3 $4" = "clone --bare --quiet https://github.com/acme/widgets" ]; then
	printf '%s %s\n' "$LEGION_PROVISIONING_TOKEN_FILE" "$(cat "$LEGION_PROVISIONING_TOKEN_FILE")" >> "$WINIT_CREDENTIAL_LOG"
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
		args func(v *treeVolume) []string
		env  func(t *testing.T, v *treeVolume)
		code int
		says func(v *treeVolume) string
	}{
		{
			name: "a --repo that is not owner/name",
			args: func(v *treeVolume) []string { return []string{"fetch", "--repo", "acme", "--feed", v.feed} },
			code: 1,
			says: func(*treeVolume) string { return `--repo must be "owner/name" (got "acme")` },
		},
		{
			name: "a --repo with a .. segment",
			args: func(v *treeVolume) []string { return []string{"fetch", "--repo", "../x", "--feed", v.feed} },
			code: 1,
			says: func(*treeVolume) string { return `--repo "../x" has a ".." segment` },
		},
		{
			name: "a --repo holding whitespace",
			args: func(v *treeVolume) []string { return []string{"fetch", "--repo", "acme/wid gets", "--feed", v.feed} },
			code: 1,
			says: func(*treeVolume) string { return `--repo "acme/wid gets" holds whitespace` },
		},
		{
			// --repo is read first: the feed and the token are wrong too.
			name: "a bad --repo, before every other input",
			args: func(*treeVolume) []string { return []string{"fetch", "--repo", "acme/..", "--feed", "feed"} },
			env:  func(t *testing.T, _ *treeVolume) { unsetenv(t, "LEGION_PROVISION_TOKEN_FILE") },
			code: 1,
			says: func(*treeVolume) string { return `--repo "acme/.." has a ".." segment` },
		},
		{
			name: "a relative --feed",
			args: func(*treeVolume) []string { return []string{"fetch", "--repo", winitRepo, "--feed", "feed"} },
			code: 1,
			says: func(*treeVolume) string { return `--feed must be an absolute path (got "feed")` },
		},
		{
			name: "LEGION_PROVISION_TOKEN_FILE unset",
			env:  func(t *testing.T, _ *treeVolume) { unsetenv(t, "LEGION_PROVISION_TOKEN_FILE") },
			code: 1,
			says: func(*treeVolume) string { return "LEGION_PROVISION_TOKEN_FILE is not set" },
		},
		{
			name: "a provisioning token file that is absent",
			env: func(t *testing.T, v *treeVolume) {
				t.Setenv("LEGION_PROVISION_TOKEN_FILE", v.token+".absent")
			},
			code: 1,
			says: func(v *treeVolume) string {
				return "LEGION_PROVISION_TOKEN_FILE names " + v.token + ".absent, which could not be read"
			},
		},
		{
			name: "a provisioning token file that is blank",
			env: func(t *testing.T, v *treeVolume) {
				if err := os.WriteFile(v.token, []byte(" \n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			code: 1,
			says: func(v *treeVolume) string { return "LEGION_PROVISION_TOKEN_FILE names " + v.token + ", which is empty" },
		},
		{
			name: "no git on PATH",
			env:  func(t *testing.T, _ *treeVolume) { t.Setenv("PATH", t.TempDir()) },
			code: 1,
			says: func(*treeVolume) string { return "git is not on PATH" },
		},
		{
			name: "an unknown flag",
			args: func(v *treeVolume) []string { return append(v.fetchArgs(), "--root", v.root) },
			code: 2,
			says: func(*treeVolume) string { return "-root" },
		},
		{
			name: "a positional argument",
			args: func(v *treeVolume) []string { return append(v.fetchArgs(), "extra") },
			code: 2,
			says: func(*treeVolume) string { return `unexpected argument "extra"` },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newTreeVolume(t)
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
	v := newTreeVolume(t).withRemote(t)
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
