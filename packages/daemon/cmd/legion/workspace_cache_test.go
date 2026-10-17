package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/repocache"
)

// cmdCacheToken is the bearer token the cache command's tests hand the cache, and the fetch's
// present.
const cmdCacheToken = "cache-token-of-acme-widgets"

// writeCacheToken is a file, outside the issue volume and TMPDIR, holding body.
func writeCacheToken(t *testing.T, body string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "cache-token")
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

// cacheArgs are a cache pod's container's: the volume's root, and an address of the test's own.
func (v *issueVolume) cacheArgs() []string {
	return []string{"cache", "--repo", winitRepo, "--root", v.root, "--listen", "127.0.0.1:0"}
}

// Every refusal `cache` makes happens before it runs git, listens or writes anything: its root is
// as it was, and no one-shot credential is left in its TMPDIR.
func TestWorkspaceInitCacheRefusesBeforeServing(t *testing.T) {
	for _, tc := range []struct {
		name string
		args func(v *issueVolume) []string
		env  func(t *testing.T, v *issueVolume)
		code int
		says func(v *issueVolume) string
	}{
		{
			name: "no --repo",
			args: func(v *issueVolume) []string { return []string{"cache", "--root", v.root} },
			code: 1,
			says: func(*issueVolume) string { return `--repo must be "owner/name" (got "")` },
		},
		{
			name: "a --repo with a .. segment",
			args: func(v *issueVolume) []string { return []string{"cache", "--repo", "../x", "--root", v.root} },
			code: 1,
			says: func(*issueVolume) string { return `--repo "../x" has a ".." segment` },
		},
		{
			// --repo is read first: the root and the token are wrong too.
			name: "a bad --repo, before every other input",
			args: func(*issueVolume) []string { return []string{"cache", "--repo", "acme", "--root", "cache"} },
			env:  func(t *testing.T, _ *issueVolume) { unsetenv(t, "LEGION_PROVISION_TOKEN_FILE") },
			code: 1,
			says: func(*issueVolume) string { return `--repo must be "owner/name" (got "acme")` },
		},
		{
			name: "a relative --root",
			args: func(*issueVolume) []string { return []string{"cache", "--repo", winitRepo, "--root", "cache"} },
			code: 1,
			says: func(*issueVolume) string { return `--root must be an absolute path (got "cache")` },
		},
		{
			name: "a --refresh that is not positive",
			args: func(v *issueVolume) []string { return append(v.cacheArgs(), "--refresh", "0s") },
			code: 1,
			says: func(*issueVolume) string { return "--refresh must be positive (got 0s)" },
		},
		{
			name: "LEGION_REPOSITORY_CACHE_TOKEN_FILE unset",
			env:  func(t *testing.T, _ *issueVolume) { unsetenv(t, "LEGION_REPOSITORY_CACHE_TOKEN_FILE") },
			code: 1,
			says: func(*issueVolume) string { return "LEGION_REPOSITORY_CACHE_TOKEN_FILE is not set" },
		},
		{
			name: "a bearer token file that is blank",
			env: func(t *testing.T, _ *issueVolume) {
				t.Setenv("LEGION_REPOSITORY_CACHE_TOKEN_FILE", writeCacheToken(t, " \n"))
			},
			code: 1,
			says: func(*issueVolume) string { return "LEGION_REPOSITORY_CACHE_TOKEN_FILE names " },
		},
		{
			name: "LEGION_PROVISION_TOKEN_FILE unset",
			env:  func(t *testing.T, _ *issueVolume) { unsetenv(t, "LEGION_PROVISION_TOKEN_FILE") },
			code: 1,
			says: func(*issueVolume) string { return "LEGION_PROVISION_TOKEN_FILE is not set" },
		},
		{
			name: "a provisioning token file that is absent",
			env:  func(t *testing.T, v *issueVolume) { t.Setenv("LEGION_PROVISION_TOKEN_FILE", v.token+".absent") },
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
			args: func(v *issueVolume) []string { return append(v.cacheArgs(), "--feed", v.feed) },
			code: 2,
			says: func(*issueVolume) string { return "-feed" },
		},
		{
			name: "a positional argument",
			args: func(v *issueVolume) []string { return append(v.cacheArgs(), "extra") },
			code: 2,
			says: func(*issueVolume) string { return `unexpected argument "extra"` },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newIssueVolume(t).withRemote(t, "main")
			v.setenv(t)
			t.Setenv("LEGION_PROVISION_TOKEN_FILE", v.token)
			t.Setenv("LEGION_REPOSITORY_CACHE_TOKEN_FILE", writeCacheToken(t, cmdCacheToken))
			if tc.env != nil {
				tc.env(t, v)
			}
			args := v.cacheArgs()
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
			if entries, err := os.ReadDir(v.root); err != nil || len(entries) != 0 {
				t.Fatalf("the root holds %v (%v), want it as it was", entries, err)
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

// `cache` serves the repository's cache until its context ends: it says where it listens, clones
// the repository onto its root with the provisioning token handed to git as a one-shot credential
// under its TMPDIR, answers /healthz ready at the remote's head to a request carrying no token,
// holds no token on the volume, and
// exits 0 once stopped.
func TestWorkspaceInitCacheServesTheRepositoryUntilStopped(t *testing.T) {
	v := newIssueVolume(t).withRemote(t, "main")
	v.setenv(t)
	t.Setenv("LEGION_PROVISION_TOKEN_FILE", v.token)
	t.Setenv("LEGION_REPOSITORY_CACHE_TOKEN_FILE", writeCacheToken(t, cmdCacheToken+"\n"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr lockedBuffer
	exited := make(chan int, 1)
	go func() {
		exited <- run(ctx, append([]string{"legion", "workspace-init"}, v.cacheArgs()...), &stdout, &stderr)
	}()

	address := ""
	deadline := time.Now().Add(winitWait)
	for address == "" {
		for _, line := range strings.Split(stdout.String(), "\n") {
			if found, ok := strings.CutPrefix(line, "workspace-init cache: serving acme/widgets on "); ok {
				address = found
			}
		}
		if address == "" {
			if time.Now().After(deadline) {
				t.Fatalf("cache never said where it serves: stdout %q, stderr %q", stdout.String(), stderr.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	var health repocache.Health
	for !health.Ready {
		request, err := http.NewRequest(http.MethodGet, "http://"+address+"/healthz", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		err = json.NewDecoder(response.Body).Decode(&health)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the cache never became ready: %+v; stdout %q, stderr %q", health, stdout.String(), stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if want := v.git(t, "--git-dir="+v.env["WINIT_REMOTE"], "rev-parse", "main"); health.Head != want || health.Repo != winitRepo {
		t.Fatalf("/healthz = %+v, want acme/widgets at the remote's main %s", health, want)
	}
	if main := v.git(t, "--git-dir="+filepath.Join(v.root, "acme", "widgets.git"), "log", "-1", "--format=%s", "refs/heads/main"); main != "seed" {
		t.Fatalf("the cache's main is %q, want the remote's", main)
	}
	credential, err := os.ReadFile(v.credentialLog)
	if err != nil {
		t.Fatalf("the clone recorded no credential: %v", err)
	}
	tokenFile, held, _ := strings.Cut(strings.TrimSpace(string(credential)), " ")
	if held != "ghs_test" || !strings.HasPrefix(tokenFile, v.tmp+string(filepath.Separator)) {
		t.Fatalf("the clone was handed %s holding %q, want the provisioning token under TMPDIR %s", tokenFile, held, v.tmp)
	}
	holdsNoToken(t, v.root, "while the cache serves")

	cancel()
	select {
	case code := <-exited:
		if code != 0 {
			t.Fatalf("cache exited %d once stopped, stderr %q", code, stderr.String())
		}
	case <-time.After(winitWait):
		t.Fatal("cache did not stop with its context")
	}
	if entries, err := os.ReadDir(v.tmp); err != nil || len(entries) != 0 {
		t.Fatalf("TMPDIR holds %v (%v) once the cache stopped, want its credential gone", entries, err)
	}
}
