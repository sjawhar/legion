package shim_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testbin"
)

// A release carries at most 40 MiB, and a workspace whose unpushed work is larger stops the
// release rather than sending it. The shim measures the bundle before it sends anything and
// answers the size it refused, which is the reason the daemon records.
func TestABundleOverTheReleasesBoundIsRefusedWithItsSize(t *testing.T) {
	testbin.RequireJJFilters(t)
	workspace := bundledWorkspace(t)

	path := socketPath(t)
	daemon := listen(t, path)
	child := newOMP(t)
	cfg := config(t, path, child)
	cfg.Env = append(environWithout("LEGION_WORKSPACE"), fakeOMPEnv+"=1", "FAKE_OMP_MARKER="+child.marker,
		"FAKE_OMP_LOG="+child.log, "LEGION_WORKSPACE="+workspace, "JJ_USER=tester", "JJ_EMAIL=tester@example.test")
	run(t, cfg, newClock())
	p := daemon.accept(t)
	p.open(t)

	// What this workspace bundles to, under a bound it fits: the save itself.
	p.send(t, shimwire.BundleWorkspace{ID: "bundle-1", Issue: "INFRA-945", MaxBytes: 40 << 20, TimeoutMs: 120_000})
	got, ok := p.next(t).(shimwire.BundleWorkspaceResult)
	if !ok || !got.OK {
		t.Fatalf("the shim answered %#v", got)
	}
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
	size := got.SizeBytes
	if size == 0 || len(got.Bundle) != size || got.Head == "" || len(got.Tips) == 0 {
		t.Fatalf("the save is %d bytes saying %d, on %s with heads %v", len(got.Bundle), size, got.Head, got.Tips)
	}

	// One byte under it: the save is built and then refused, with the size the daemon records.
	p.send(t, shimwire.BundleWorkspace{ID: "bundle-2", Issue: "INFRA-945", MaxBytes: size - 1, TimeoutMs: 120_000})
	got, ok = p.next(t).(shimwire.BundleWorkspaceResult)
	if !ok {
		t.Fatalf("the shim answered %#v", got)
	}
	if got.OK || len(got.Bundle) != 0 {
		t.Fatalf("the shim sent a bundle over the bound: ok=%v, %d bytes", got.OK, len(got.Bundle))
	}
	if got.SizeBytes != size {
		t.Fatalf("the refusal names %d bytes and reads %q, want the %d the bundle is", got.SizeBytes, got.Error, size)
	}
	if !strings.Contains(got.Error, strconv.Itoa(size)) || !strings.Contains(got.Error, strconv.Itoa(size-1)) {
		t.Fatalf("the refusal reads %q, want it to name the size and the bound", got.Error)
	}
	// Nothing of the save is left in the workspace: the bundle is built under the temporary
	// directory, and the refs it needed are gone.
	entries, err := os.ReadDir(workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".bundle") {
			t.Fatalf("the save left %s in the workspace", entry.Name())
		}
	}
	if refs := jjGit(t, workspace, "for-each-ref", "--format=%(refname)", "refs/legion-bundle/"); refs != "" {
		t.Fatalf("the save left its temporary refs behind:\n%s", refs)
	}
}

// A shim with no workspace to bundle refuses by name rather than answering an empty bundle.
func TestABundleOfNoWorkspaceIsRefusedByName(t *testing.T) {
	path := socketPath(t)
	daemon := listen(t, path)
	child := newOMP(t)
	cfg := config(t, path, child)
	cfg.Env = append(environWithout("LEGION_WORKSPACE"), fakeOMPEnv+"=1", "FAKE_OMP_MARKER="+child.marker, "FAKE_OMP_LOG="+child.log)
	run(t, cfg, newClock())
	p := daemon.accept(t)
	p.open(t)
	p.send(t, shimwire.BundleWorkspace{ID: "bundle-3", Issue: "INFRA-945", MaxBytes: 40 << 20, TimeoutMs: 10_000})
	got, ok := p.next(t).(shimwire.BundleWorkspaceResult)
	if !ok || got.OK || !strings.Contains(got.Error, "LEGION_WORKSPACE") {
		t.Fatalf("the shim answered %#v, want a refusal naming LEGION_WORKSPACE", got)
	}
}

// bundledWorkspace is a colocated jj workspace with one unpushed commit and a working-copy change
// of its own: what a released pod's workspace holds, in miniature.
func bundledWorkspace(t *testing.T) string {
	t.Helper()
	for _, tool := range []string{"jj", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is required: %v", tool, err)
		}
	}
	root := t.TempDir()
	// The host's own jj configuration (its user, its trailers) must not reach the fixture.
	config := filepath.Join(root, "jj.toml")
	if err := os.WriteFile(config, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JJ_CONFIG", config)
	workspace := filepath.Join(root, "ws")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	jjGit(t, workspace, "init", "--quiet", "-b", "main", workspace)
	if err := os.WriteFile(filepath.Join(workspace, "f.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	jjGit(t, workspace, "add", "-A")
	jjGit(t, workspace, "-c", "user.name=tester", "-c", "user.email=tester@example.test", "commit", "--quiet", "-m", "one")
	jjRun(t, workspace, "git", "init", "--colocate", workspace)
	jjRun(t, workspace, "new", "-R", workspace, "-m", "unpushed")
	if err := os.WriteFile(filepath.Join(workspace, "wip.txt"), []byte(strings.Repeat("work\n", 4096)), 0o644); err != nil {
		t.Fatal(err)
	}
	return workspace
}

// jjRun runs one jj command in the workspace it reads, as every jj a test starts must
// (docs/testing.md).
func jjRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return workspaceCommand(t, dir, append([]string{"jj"}, args...))
}

func jjGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return workspaceCommand(t, dir, append([]string{"git"}, args...))
}

func workspaceCommand(t *testing.T, dir string, args []string) string {
	t.Helper()
	command := exec.Command(args[0], args[1:]...)
	command.Dir = dir
	command.Env = append(os.Environ(), "JJ_USER=tester", "JJ_EMAIL=tester@example.test",
		"GIT_AUTHOR_NAME=tester", "GIT_AUTHOR_EMAIL=tester@example.test",
		"GIT_COMMITTER_NAME=tester", "GIT_COMMITTER_EMAIL=tester@example.test")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s in %s: %v\n%s", strings.Join(args, " "), dir, err, output)
	}
	return strings.TrimSpace(string(output))
}
