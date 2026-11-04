// Package rescuetest builds an issue pod's disk for the push check's tests: a bare repository
// standing in for GitHub, and on the disk a jj clone of it with the issue's jj workspace, laid out
// as workspace.Location lays a pod's out. Every command is the real git and jj from PATH, under
// fixtureenv.Isolated: no workspace, configuration, remote or credential of whoever runs the tests.
package rescuetest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/rescue"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// Owner and Repo are the repository the disk's clone is of.
const (
	Owner = "acme"
	Repo  = "widgets"
)

// Disk is one pod's disk and the remote its clone pushes to.
type Disk struct {
	t testing.TB
	// Remote is the bare repository standing in for GitHub.
	Remote string
	// StateDir is the disk's root, a pod's /legion.
	StateDir string
	// Clone is the shared clone, a colocated jj repository whose origin is Remote.
	Clone string
	// Workspace is the issue's jj workspace of Clone.
	Workspace string
	// Issue is the issue key, and Branch its branch, which Remote holds at the base commit.
	Issue, Branch string
	// Base is the commit Remote's main and the issue's branch both hold at the start.
	Base string
	// Env is what every command of the disk runs in, and what the push check runs in.
	Env []string
	// Provisioned is when the pod was made: New takes it before anything else, and records it as
	// the workspace's creation in the workspace's .jj (workspace.RecordCreated), which the
	// launcher's resume check reads. RunBefore dates a commit an hour before it.
	Provisioned time.Time
}

// New is a disk whose remote holds one commit on main and on the issue's branch, and whose
// workspace stands on that commit with nothing in it the remote lacks. Each of before runs once the
// remote holds that commit and before the pod's clone is made, so what it pushes to the remote the
// clone holds from the start; the clone records what it held then, as provisioning records it
// (workspace.RecordHeld).
func New(t testing.TB, issue string, before ...func(d *Disk)) *Disk {
	t.Helper()
	for _, tool := range []string{"git", "jj"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("the push check's tests drive a real %s: %v", tool, err)
		}
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	d := &Disk{
		t:         t,
		Remote:    filepath.Join(root, "remote.git"),
		StateDir:  filepath.Join(root, "legion"),
		Issue:     issue,
		Branch:    "legion/" + issue,
		Env:       fixtureenv.Isolated(home),
		Clone:     filepath.Join(root, "legion", "repos", "github.com", Owner, Repo),
		Workspace: filepath.Join(root, "legion", "workspaces", Owner, Repo, strings.ToLower(issue)),
	}
	d.Provisioned = time.Now()
	seed := filepath.Join(root, "seed")
	d.Run(root, "git", "init", "--quiet", "--bare", "--initial-branch=main", d.Remote)
	d.Run(root, "git", "init", "--quiet", "--initial-branch=main", seed)
	d.Write(seed, "base.txt", "base\n")
	d.Run(seed, "git", "add", "base.txt")
	d.Run(seed, "git", "commit", "--quiet", "-m", "base")
	d.Run(seed, "git", "push", "--quiet", d.Remote, "main", "main:refs/heads/"+d.Branch)
	d.Base = d.Run(seed, "git", "rev-parse", "HEAD")
	for _, seedOrigin := range before {
		seedOrigin(d)
	}
	if err := os.MkdirAll(filepath.Dir(d.Clone), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(d.Workspace), 0o755); err != nil {
		t.Fatal(err)
	}
	d.Run(root, "jj", "git", "clone", "--colocate", d.Remote, d.Clone)
	if err := workspace.RecordHeld(context.Background(), workspace.NewRunner(time.Minute, d.tools()), d.Clone); err != nil {
		t.Fatal(err)
	}
	d.Run(d.Clone, "jj", "workspace", "add", "--name", strings.ToLower(issue), d.Workspace)
	if err := workspace.RecordCreated(d.Workspace, d.Provisioned); err != nil {
		t.Fatal(err)
	}
	d.JJ("bookmark", "track", d.Branch, "--remote", "origin")
	return d
}

// Run runs argv in dir under the disk's environment and returns its trimmed output, failing the
// test when it fails.
func (d *Disk) Run(dir string, argv ...string) string {
	d.t.Helper()
	return d.run(dir, nil, argv...)
}

// RunBefore is Run for a commit someone made before the pod provisioned the workspace: git's
// author and committer dates, and jj's (JJ_TIMESTAMP), are an hour before Provisioned, as a commit
// the clone holds from a fetch of something older than the pod is.
func (d *Disk) RunBefore(dir string, argv ...string) string {
	d.t.Helper()
	before := d.Provisioned.Add(-time.Hour).Format(time.RFC3339)
	return d.run(dir, []string{"GIT_AUTHOR_DATE=" + before, "GIT_COMMITTER_DATE=" + before, "JJ_TIMESTAMP=" + before}, argv...)
}

func (d *Disk) run(dir string, env []string, argv ...string) string {
	d.t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(slices.Clone(d.Env), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		d.t.Fatalf("%s (in %s): %v\n%s", strings.Join(argv, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// JJ runs jj in the workspace, as a role does: its working copy snapshotted first.
func (d *Disk) JJ(args ...string) string {
	d.t.Helper()
	return d.Run(d.Workspace, append([]string{"jj"}, args...)...)
}

// Git runs git in the workspace.
func (d *Disk) Git(args ...string) string {
	d.t.Helper()
	return d.Run(d.Workspace, append([]string{"git"}, args...)...)
}

// Write writes a file under dir.
func (d *Disk) Write(dir, name, body string) {
	d.t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		d.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		d.t.Fatal(err)
	}
}

// RemoteBranch is the commit the remote's branch holds, and "" when the remote has no such branch.
func (d *Disk) RemoteBranch(branch string) string {
	d.t.Helper()
	cmd := exec.Command("git", "--git-dir="+d.Remote, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Env = d.Env
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

// RemoteBranches is every branch the remote has.
func (d *Disk) RemoteBranches() []string {
	d.t.Helper()
	return strings.Fields(d.Run(d.Remote, "git", "for-each-ref", "--format=%(refname:short)", "refs/heads"))
}

// RefuseWrites makes the remote refuse every push, as a remote that refuses the role's credential
// would: its pre-receive hook says why and fails. Reads of the remote still work. The hook is the
// remote's own, which git runs on the remote's side whatever configuration the pushing side pins.
func (d *Disk) RefuseWrites() {
	d.t.Helper()
	hook := filepath.Join(d.Remote, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'the remote refuses this push' >&2\nexit 1\n"), 0o755); err != nil {
		d.t.Fatal(err)
	}
}

// Check runs the push check over the disk as a role's shim runs it (shim.rescue), under the
// disk's environment, with rescue branches named from prefix.
func (d *Disk) Check(ctx context.Context, prefix string) []shimwire.RescueWorkspace {
	d.t.Helper()
	return rescue.Run(ctx, rescue.Request{
		StateDir: d.StateDir, Branch: prefix, Env: d.Env,
		WorkspaceTimeout: time.Minute, Deadline: time.Now().Add(2 * time.Minute),
	})
}

// tools is the git and jj on PATH, as boot resolves them for provisioning's runner.
func (d *Disk) tools() map[string]string {
	d.t.Helper()
	tools := map[string]string{}
	for _, tool := range []string{"git", "jj"} {
		path, err := exec.LookPath(tool)
		if err != nil {
			d.t.Fatalf("the push check's tests drive a real %s: %v", tool, err)
		}
		tools[tool] = path
	}
	return tools
}
