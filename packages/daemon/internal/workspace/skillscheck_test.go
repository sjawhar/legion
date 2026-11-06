package workspace

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// installedCopy is the plugin tree root as Oh My Pi installs it: a copy of the clone's plugin
// directory, which is what `omp plugin install` leaves for a local marketplace (measured against
// omp 18.16 with a local marketplace of one plugin: the installed directory and the source
// directory compared equal, `diff -r` silent). A test that wants a mismatch edits the copy.
func installedCopy(t *testing.T, source string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "installed")
	if err := os.CopyFS(root, os.DirFS(source)); err != nil {
		t.Fatal(err)
	}
	return root
}

// clonedSkills is a skills volume a CloneSkills run filled: the clone, the four records, and the
// path of each plugin's directory in that clone. The remote holds two plugins, so a test can break
// the second and see the first pass.
func clonedSkills(t *testing.T) (dir string, report SkillsReport, sources map[string]string) {
	t.Helper()
	run := newLocalRunner(t)
	request := skillsRequest(t)
	remote, _ := seedSkillsRemote(t, ".claude-plugin/marketplace.json", testCatalog)
	run.remote = remote
	report, err := CloneSkills(context.Background(), run, request)
	if err != nil {
		t.Fatalf("CloneSkills: %v", err)
	}
	clone := SkillsClone(request.Dir, request.Repo)
	sources = map[string]string{}
	for _, plugin := range report.Plugins {
		sources[plugin] = filepath.Join(clone, "plugins", plugin)
	}
	return request.Dir, report, sources
}

// CloneSkills records a digest of every plugin's tree beside the clone, one `<sha256>  <plugin>`
// line per plugin in the catalog's order, and each digest is the digest of that plugin's own
// directory of the clone: the number a role's check compares its installed plugin against.
func TestCloneSkillsRecordsEachPluginsTreeDigest(t *testing.T) {
	dir, report, sources := clonedSkills(t)
	digests, order, err := ReadSkillsTrees(dir)
	if err != nil {
		t.Fatalf("ReadSkillsTrees: %v", err)
	}
	if !slices.Equal(order, report.Plugins) {
		t.Errorf("the record lists %q, want the catalog's order %q", order, report.Plugins)
	}
	for _, plugin := range report.Plugins {
		want, err := SkillsTreeDigest(sources[plugin])
		if err != nil {
			t.Fatal(err)
		}
		if digests[plugin] != want {
			t.Errorf("the record holds %s for %q, and its directory digests to %s", digests[plugin], plugin, want)
		}
		if report.Trees[plugin] != want {
			t.Errorf("the report holds %s for %q, want %s", report.Trees[plugin], plugin, want)
		}
	}
	// Two different trees must not digest alike, or the check would pass on a swap.
	if len(report.Plugins) > 1 && digests[report.Plugins[0]] == digests[report.Plugins[1]] {
		t.Errorf("both plugins digest to %s, so the check could not tell one from the other", digests[report.Plugins[0]])
	}
}

// The passing arm: an install whose files are the clone's plugin directory, which is what Oh My Pi
// leaves for a local marketplace, matches the recorded digest for every plugin.
func TestCheckSkillsInstalledPassesOnTheCommitThePodCloned(t *testing.T) {
	dir, report, sources := clonedSkills(t)
	installed := map[string]string{}
	for _, plugin := range report.Plugins {
		installed[plugin] = installedCopy(t, sources[plugin])
	}
	if err := CheckSkillsInstalled(dir, installed); err != nil {
		t.Fatalf("CheckSkillsInstalled on the pod's own commit = %v, want it to pass", err)
	}
}

// The failing arm, one case per way an install can stop being the commit the pod cloned: a file
// changed, a file added, a file removed, a plugin the install left nowhere, and a plugin installed
// that the records do not name. Each names the plugin, and none passes.
func TestCheckSkillsInstalledRefusesAnythingButTheCommitThePodCloned(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(t *testing.T, installed map[string]string, plugin string)
		want string
	}{
		"a file changed": {
			func(t *testing.T, installed map[string]string, plugin string) {
				path := filepath.Join(installed[plugin], "skills", "example.md")
				if err := os.WriteFile(path, []byte("not what the commit holds\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			"is not what the pod's skills clone recorded",
		},
		"a file added": {
			func(t *testing.T, installed map[string]string, plugin string) {
				if err := os.WriteFile(filepath.Join(installed[plugin], "planted.md"), []byte("planted\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			"is not what the pod's skills clone recorded",
		},
		"a file removed": {
			func(t *testing.T, installed map[string]string, plugin string) {
				if err := os.Remove(filepath.Join(installed[plugin], "skills", "example.md")); err != nil {
					t.Fatal(err)
				}
			},
			"is not what the pod's skills clone recorded",
		},
		"a plugin installed nowhere": {
			func(t *testing.T, installed map[string]string, plugin string) { delete(installed, plugin) },
			"is not installed: Oh My Pi reports no directory for it",
		},
		"a plugin the records do not name": {
			func(t *testing.T, installed map[string]string, plugin string) {
				installed["unrecorded"] = installed[plugin]
			},
			"the pod cannot say which commit its files came from",
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, report, sources := clonedSkills(t)
			installed := map[string]string{}
			for _, plugin := range report.Plugins {
				installed[plugin] = installedCopy(t, sources[plugin])
			}
			// Break the last plugin of the catalog, so a check that stopped at the first would
			// pass and be caught here.
			broken := report.Plugins[len(report.Plugins)-1]
			tc.edit(t, installed, broken)
			err := CheckSkillsInstalled(dir, installed)
			if err == nil {
				t.Fatalf("CheckSkillsInstalled = nil, want a refusal naming %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CheckSkillsInstalled = %v, want a refusal naming %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), broken) && !strings.Contains(err.Error(), "unrecorded") {
				t.Errorf("the refusal %v does not name the plugin %q", err, broken)
			}
		})
	}
}

// A plugin tree holding a symlink is refused rather than digested: the digest would then be a
// statement about whatever the link points at, and a link is how a plugin could carry something
// from outside the clone into what a role loads.
func TestSkillsTreeDigestRefusesALinkInThePluginTree(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "real.md"), []byte("real\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hostname", filepath.Join(root, "link.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := SkillsTreeDigest(root); err == nil || !strings.Contains(err.Error(), "may hold only directories and regular files") {
		t.Fatalf("SkillsTreeDigest = %v, want the link refused", err)
	}
}

// The digest is of the paths and the bytes, so a file moved between directories changes it even
// when every byte in the tree is the same: a check that hashed contents alone would pass a tree
// whose skills had been rearranged into something an agent reads differently.
func TestSkillsTreeDigestFollowsPathsAndNotJustBytes(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	for root, name := range map[string]string{first: "a.md", second: "b.md"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("same bytes\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	a, err := SkillsTreeDigest(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := SkillsTreeDigest(second)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Errorf("both trees digest to %s, so a renamed file would pass the check", a)
	}
}

// The digest belongs to one tree and no other. The collision review of pull request 2806 found
// needs a newline in a path: writing the fields merely separated, as `<path>\n<size>\n<content>`
// per file, the two-file tree {"a": "x", "b": "Y"} and the one-file tree {"a\n1\nxb": "Y"} write
// the same bytes (checked arithmetically: both hash to 87d2e9a731e47dae… under that format, and to
// different digests under this one). So the first case below is that exact name, refused, which is
// what closes it; the second is a pair with no newline anywhere, which the length prefixes keep
// apart.
func TestSkillsTreeDigestBelongsToOneTree(t *testing.T) {
	twoFiles := t.TempDir()
	for name, content := range map[string]string{"a": "x", "b": "Y"} {
		if err := os.WriteFile(filepath.Join(twoFiles, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The name the separated format would have let collide with the tree above.
	oneFile := t.TempDir()
	if err := os.WriteFile(filepath.Join(oneFile, "a\n1\nxb"), []byte("Y"), 0o644); err != nil {
		t.Skipf("this filesystem does not take a newline in a file name: %v", err)
	}
	if _, err := SkillsTreeDigest(oneFile); err == nil || !strings.Contains(err.Error(), "newline or a NUL") {
		t.Fatalf("SkillsTreeDigest of a tree whose file name holds a newline = %v, want it refused", err)
	}
	// And the prefixes keep two trees apart with no newline anywhere: a tree's one file named `ab`
	// holding `cd`, against one named `a` holding `bcd`, whose names and contents concatenate to
	// the same bytes.
	first, second := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(first, "ab"), []byte("cd"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "a"), []byte("bcd"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := SkillsTreeDigest(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := SkillsTreeDigest(second)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Errorf("{ab: cd} and {a: bcd} both digest to %s, so the digest does not belong to one tree", a)
	}
}

// A file's executable bit is part of the digest: a skill's helper script that comes out not
// executable is a different thing from one that does. Oh My Pi's install preserves the mode, which
// is measured in a pod of the pinned image rather than assumed, so hashing the bit cannot fail a
// launch on a difference the install made.
func TestSkillsTreeDigestFollowsTheExecutableBit(t *testing.T) {
	plain, executable := t.TempDir(), t.TempDir()
	for root, mode := range map[string]os.FileMode{plain: 0o644, executable: 0o755} {
		if err := os.WriteFile(filepath.Join(root, "run.sh"), []byte("#!/bin/sh\necho hi\n"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(root, "run.sh"), mode); err != nil {
			t.Fatal(err)
		}
	}
	a, err := SkillsTreeDigest(plain)
	if err != nil {
		t.Fatal(err)
	}
	b, err := SkillsTreeDigest(executable)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Errorf("a 0644 and a 0755 copy of the same file both digest to %s", a)
	}
}

// A malformed or missing TREES record is a refusal naming it, not a check that silently passes: a
// pod whose record cannot be read cannot say what its agent loads.
func TestReadSkillsTreesRefusesARecordItCannotRead(t *testing.T) {
	for name, tc := range map[string]struct{ content, want string }{
		"missing":               {"", "read the skills record"},
		"a line with no digest": {"  one\n", "want `<sha256>  <plugin>`"},
		"a line with no plugin": {"abc  \n", "want `<sha256>  <plugin>`"},
		"one plugin twice":      {"abc  one\ndef  one\n", "names plugin \"one\" twice"},
		"no plugin":             {"\n", "want `<sha256>  <plugin>`"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.content != "" {
				if err := os.WriteFile(SkillsTreesFile(dir), []byte(tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := ReadSkillsTrees(dir); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ReadSkillsTrees = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}
