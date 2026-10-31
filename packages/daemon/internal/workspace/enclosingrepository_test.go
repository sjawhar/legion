package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Provisioning leaves the repository it happens to stand inside alone, on both ways to the
// repository, and refuses outright to provision below one.
//
// jj searches upward from the directory it runs in for the closest `.jj` and opens that
// repository; it has no ceiling option, and the two provisioning commands that create a
// repository — `jj git clone` and `jj git init --colocate` — open no workspace and so can take no
// -R. A Legion issue pod runs these tests from inside $LEGION_WORKSPACE, a jj workspace of the
// pod's real shared clone, so when a provisioning child inherited the test process's working
// directory, jj opened and wrote that clone (dispatch://INFRA-623). The decoy below stands in for
// the pod's clone and t.Chdir puts the test where the pod's test process stands, which is the
// whole of what a GitHub runner cannot reproduce: its checkout has no `.jj` anywhere above it.
//
// Half (a), with the state directory outside the decoy, holds every call site to a directory of
// its own. Half (b), with the state directory inside it, is what holds the ancestor walk: a
// directory the caller named has ancestors too, and jj still walks up through them.
func TestProvisionLeavesTheEnclosingRepositoryUntouched(t *testing.T) {
	for _, source := range []string{"github", "feed"} {
		t.Run("a state directory outside it, from "+source, func(t *testing.T) {
			run := newLocalRunner(t)
			decoy := enclosingRepository(t)
			before := readEnclosingState(t, decoy)
			t.Chdir(decoy)

			req := provisionRequest(t)
			if source == "feed" {
				req = feedRequest(t, run, req)
			}
			working, err := Provision(context.Background(), run, req)
			if err != nil {
				t.Fatalf("Provision from %s while standing in %s: %v", source, decoy, err)
			}
			if _, err := os.Stat(filepath.Join(working.Dir, ".jj")); err != nil {
				t.Errorf("Provision from %s left no workspace of its own: %v", source, err)
			}
			assertEnclosingRepositoryUnchanged(t, decoy, before)
		})
	}

	t.Run("a state directory inside it", func(t *testing.T) {
		run := newLocalRunner(t)
		decoy := enclosingRepository(t)
		before := readEnclosingState(t, decoy)
		t.Chdir(decoy)

		req := provisionRequest(t)
		req.StateDir = filepath.Join(decoy, "legion state")
		req.Source = FromGitHub(staticToken(testInstallationToken), req.StateDir)
		working, err := Provision(context.Background(), run, req)
		if err == nil {
			t.Fatalf("Provision under the enclosing repository made %s, want a refusal: the shared clone would sit below %s, and every jj that creates a repository there would open it instead", working.Clone, decoy)
		}
		if enclosing := filepath.Join(decoy, ".jj"); !strings.Contains(err.Error(), enclosing) {
			t.Errorf("Provision = %v, want a refusal naming the enclosing %s", err, enclosing)
		}
		assertEnclosingRepositoryUnchanged(t, decoy, before)
	})
}

// enclosingRepository is the decoy: a colocated jj repository holding a commit of its own and a
// bookmark on it, somewhere below the test's temporary directory, standing in for a pod's shared
// clone.
//
// Its repository configuration is the legacy `.jj/repo/config.toml`, which jj 0.38 and later
// migrate into the config home — moving the file away and writing a `config-id` beside where it
// was — the first time a jj command opens the repository. That migration is this package's own
// concern elsewhere (guardWorkspace's disarmLegacyConfig), it is the write the pod's read-only
// `/legion/repos` mount caught, and it is what makes "jj opened this repository" visible in the
// repository rather than only in jj's output: a jj that merely opens a repository with nothing to
// migrate leaves no trace in it at all.
func enclosingRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	decoy := filepath.Join(root, "enclosing")
	runSetup(t, root, "jj", "git", "init", "--colocate", decoy)
	if err := os.WriteFile(filepath.Join(decoy, "work.txt"), []byte("the enclosing repository's own work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runSetup(t, decoy, "jj", "describe", "-m", "the enclosing repository's own commit")
	runSetup(t, decoy, "jj", "bookmark", "set", "enclosing-work", "-r", "@")
	runSetup(t, decoy, "jj", "new")
	if err := os.WriteFile(filepath.Join(decoy, ".jj", "repo", "config.toml"), []byte("[user]\nname = \"the enclosing repository's owner\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return decoy
}

// enclosingState is what the decoy holds: refs, the colocated repository's bookmarks among them,
// and a content fingerprint of every file under its `.jj`.
//
// Nothing here runs jj in the decoy, because opening the decoy with jj is itself the write under
// test: `jj op log` would both migrate the repository configuration and, without
// --ignore-working-copy, snapshot the working copy, so the measurement would destroy what it
// measures. The operation log's head is read instead as the file name under
// `.jj/repo/op_heads/heads`, which the fingerprint carries along with the working-copy state, the
// operation and commit stores, and the repository configuration. Both halves are byte-stable
// across repeated reads of an untouched decoy, which is what makes a difference between them
// evidence rather than noise.
type enclosingState struct {
	refs  string
	files map[string]string
}

func readEnclosingState(t *testing.T, decoy string) enclosingState {
	t.Helper()
	state := enclosingState{refs: runSetup(t, decoy, "git", "for-each-ref"), files: map[string]string{}}
	jjDir := filepath.Join(decoy, ".jj")
	err := filepath.WalkDir(jjDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		relative, err := filepath.Rel(decoy, path)
		if err != nil {
			return err
		}
		// A `.jj` holds a symlink or a workspace pointer as readily as a regular file, and what
		// such an entry is counts as much as what a file holds.
		if !entry.Type().IsRegular() {
			state.files[relative] = entry.Type().String()
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		digest := sha256.New()
		if _, err := io.Copy(digest, file); err != nil {
			return err
		}
		state.files[relative] = hex.EncodeToString(digest.Sum(nil))
		return nil
	})
	if err != nil {
		t.Fatalf("read %s: %v", jjDir, err)
	}
	return state
}

// assertEnclosingRepositoryUnchanged reads the decoy again and names every difference from
// before: which file under `.jj` was written, lost or rewritten, so that a failure says what
// provisioning did to a repository it does not own.
func assertEnclosingRepositoryUnchanged(t *testing.T, decoy string, before enclosingState) {
	t.Helper()
	after := readEnclosingState(t, decoy)
	if before.refs != after.refs {
		t.Errorf("the enclosing repository's refs changed.\nbefore:\n%safter:\n%s", before.refs, after.refs)
	}
	paths := map[string]struct{}{}
	for path := range before.files {
		paths[path] = struct{}{}
	}
	for path := range after.files {
		paths[path] = struct{}{}
	}
	for _, path := range slices.Sorted(maps.Keys(paths)) {
		was, held := before.files[path]
		now, still := after.files[path]
		switch {
		case !held:
			t.Errorf("provisioning wrote %s into the enclosing repository", filepath.Join(decoy, path))
		case !still:
			t.Errorf("provisioning removed %s from the enclosing repository", filepath.Join(decoy, path))
		case was != now:
			t.Errorf("provisioning rewrote %s in the enclosing repository", filepath.Join(decoy, path))
		}
	}
}

// A command of provisioning's names an absolute working directory of its own or does not run at
// all: an empty Dir has the child inherit the working directory of the process that started it,
// and a relative Dir resolves against that same directory, so either way the child would run
// somewhere its caller never chose.
func TestTheRunnerRefusesACommandWithNoAbsoluteWorkingDirectory(t *testing.T) {
	for name, tc := range map[string]struct {
		dir string
		// own is a directory of the command's own, the test's temporary one, in place of dir.
		own bool
	}{
		"no directory at all":              {},
		"a relative directory":             {dir: filepath.Join("relative", "directory")},
		"an absolute directory of its own": {own: true},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			// The stub records that it ran, so a refusal is one that happened before exec rather
			// than a child that ran and failed.
			marker := filepath.Join(dir, "ran")
			stub := filepath.Join(dir, "git")
			if err := os.WriteFile(stub, []byte("#!/bin/sh\n: > '"+marker+"'\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			command := Command{Argv: []string{"git", "status", "--porcelain"}, Dir: tc.dir, Timeout: testTimeout}
			if tc.own {
				command.Dir = dir
			}

			_, err := NewRunner(testTimeout, map[string]string{"git": stub}).Run(context.Background(), command)
			_, ran := os.Stat(marker)
			switch {
			case tc.own:
				if err != nil {
					t.Fatalf("Run in %s = %v, want the command to run", command.Dir, err)
				}
				if ran != nil {
					t.Errorf("Run in %s started nothing: %v", command.Dir, ran)
				}
			case err == nil:
				t.Fatalf("Run with Dir %q = no error, want a refusal", command.Dir)
			case !strings.Contains(err.Error(), strings.Join(command.Argv, " ")):
				t.Errorf("Run with Dir %q = %v, want a refusal naming the command", command.Dir, err)
			case ran == nil:
				t.Errorf("Run with Dir %q refused with %v after starting the command anyway", command.Dir, err)
			}
		})
	}
}

// The workspace a jj command opens is the one its -R names, and nothing else: Command.Dir is
// purely the working directory, so the two commands that create a repository of their own open no
// workspace however the caller names their directory.
func TestJJWorkspaceRootIsTheRepositoryFlagAlone(t *testing.T) {
	for name, tc := range map[string]struct {
		command Command
		want    string
	}{
		"jj git clone opens no workspace": {
			command: Command{Argv: []string{"jj", "git", "clone", "https://github.com/acme/widgets", "/state/repos/github.com/acme/widgets"}, Dir: "/state/repos/github.com"},
		},
		"jj git init --colocate opens no workspace": {
			command: Command{Argv: []string{"jj", "git", "init", "--colocate", "/state/repos/github.com/acme/temporary"}, Dir: "/state/repos/github.com"},
		},
		"-R names the workspace": {
			command: Command{Argv: []string{"jj", "log", "-R", "/state/repos/github.com/acme/widgets"}, Dir: "/elsewhere"},
			want:    "/state/repos/github.com/acme/widgets",
		},
		"--repository names the workspace": {
			command: Command{Argv: []string{"jj", "log", "--repository", "/state/workspaces/acme/widgets/widgets-42"}, Dir: "/elsewhere"},
			want:    "/state/workspaces/acme/widgets/widgets-42",
		},
		"--repository= names the workspace": {
			command: Command{Argv: []string{"jj", "log", "--repository=/state/workspaces/acme/widgets/widgets-42"}, Dir: "/elsewhere"},
			want:    "/state/workspaces/acme/widgets/widgets-42",
		},
		"the last of several wins, as it does for jj": {
			command: Command{Argv: []string{"jj", "log", "-R", "/first", "--repository", "/second", "--repository=/last"}, Dir: "/elsewhere"},
			want:    "/last",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if root := jjWorkspaceRoot(tc.command); root != tc.want {
				t.Errorf("jjWorkspaceRoot(%q, running in %s) = %q, want %q", tc.command.Argv, tc.command.Dir, root, tc.want)
			}
		})
	}
}

// A jj command that creates a repository of its own is refused below a repository it does not
// own, however far above it that repository sits, because jj would open and write that one
// instead of creating its own.
func TestAJJThatCreatesARepositoryIsRefusedInsideOne(t *testing.T) {
	for name, tc := range map[string]struct {
		// plant builds the tree below root and returns the directory the command would run in.
		plant func(t *testing.T, root string) string
		// repository is the directory holding the `.jj` the refusal must name, relative to root;
		// empty when the directory is allowed.
		repository string
	}{
		"the directory is a repository itself": {
			plant: func(t *testing.T, root string) string {
				makeDir(t, filepath.Join(root, "repository", ".jj"))
				return filepath.Join(root, "repository")
			},
			repository: "repository",
		},
		"its grandparent is a repository": {
			plant: func(t *testing.T, root string) string {
				makeDir(t, filepath.Join(root, "repository", ".jj"))
				return makeDir(t, filepath.Join(root, "repository", "state", "repos"))
			},
			repository: "repository",
		},
		"the .jj above it is jj's workspace pointer, a file": {
			plant: func(t *testing.T, root string) string {
				makeDir(t, filepath.Join(root, "workspace"))
				if err := os.WriteFile(filepath.Join(root, "workspace", ".jj"), []byte("repo: ../repository/.jj/repo\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return makeDir(t, filepath.Join(root, "workspace", "state"))
			},
			repository: "workspace",
		},
		"a directory below a repository that is not there yet": {
			plant: func(t *testing.T, root string) string {
				makeDir(t, filepath.Join(root, "repository", ".jj"))
				return filepath.Join(root, "repository", "state", "repos", "github.com")
			},
			repository: "repository",
		},
		"no repository above it": {
			plant: func(t *testing.T, root string) string { return makeDir(t, filepath.Join(root, "state", "repos")) },
		},
		"a directory that is not there yet, with no repository above it": {
			plant: func(t *testing.T, root string) string { return filepath.Join(root, "state", "repos", "github.com") },
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dir := tc.plant(t, root)

			err := refuseEnclosingWorkspace(dir)
			if tc.repository == "" {
				if err != nil {
					t.Fatalf("refuseEnclosingWorkspace(%s) = %v, want no refusal (no .jj may sit above the tests' temporary directory)", dir, err)
				}
				return
			}
			repository := filepath.Join(root, tc.repository)
			enclosing := filepath.Join(repository, ".jj")
			switch {
			case err == nil:
				t.Fatalf("refuseEnclosingWorkspace(%s) = no error, want a refusal naming %s", dir, enclosing)
			case !strings.Contains(err.Error(), dir):
				t.Errorf("refuseEnclosingWorkspace(%s) = %v, want a refusal naming the directory the command would run in", dir, err)
			case !strings.Contains(err.Error(), enclosing):
				t.Errorf("refuseEnclosingWorkspace(%s) = %v, want a refusal naming the enclosing %s", dir, err, enclosing)
			}
		})
	}
}

// makeDir is dir, with every parent of it, created.
func makeDir(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}
