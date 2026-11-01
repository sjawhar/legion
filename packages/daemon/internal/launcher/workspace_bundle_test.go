package launcher

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shim"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testbin"
)

// The daemon sends a released workspace's bundle in slices under the frame limit, and the
// launcher, which reassembles nothing of its own, puts them back together in memory: a repeated
// slice is answered as a repeated launcher_start id is, and a sequence that did not arrive as it
// was sent reaches no workspace at all.
func TestTheLauncherReassemblesABundleAndRefusesOneThatDidNotArriveAsItWasSent(t *testing.T) {
	saved := shimwire.WorkspaceBundle{
		Bundle:    bytes.Repeat([]byte("PACKjj\x00\x02"), (768<<10)/8),
		Head:      "f2f134541b30dfa5160a5d1ce5e17b76c967c661",
		Tips:      []string{"da4f9f492e9f8349389c243743e0a000cd1c676f"},
		Bookmarks: map[string]string{"legion/INFRA-945": "dffe88ae2f793e13436cef3921adc988433fbd4e"},
		At:        time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC),
	}
	frames, err := shimwire.SplitWorkspaceBundle("restore-1", saved)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) < 3 {
		t.Fatalf("the fixture sliced into %d frames; the test needs several", len(frames))
	}

	t.Run("a whole sequence is held", func(t *testing.T) {
		r := &bundleRestore{results: map[string]shimwire.LauncherBundleResult{}}
		for index, frame := range frames {
			got := r.receive(frame)
			if !got.OK || got.ID != "restore-1" || got.Index != index {
				t.Fatalf("slice %d answered %#v", index, got)
			}
			if want := index == len(frames)-1; got.Held != want {
				t.Fatalf("slice %d of %d answered held=%v, want %v: the daemon deletes its saved copy on the last slice's confirmation",
					index, len(frames), got.Held, want)
			}
		}
		read, err := shimwire.ParseWorkspaceBundle(r.body)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(read.Bundle, saved.Bundle) || read.Head != saved.Head {
			t.Fatalf("the launcher holds %d bytes on %s, want %d on %s", len(read.Bundle), read.Head, len(saved.Bundle), saved.Head)
		}
	})

	t.Run("a repeated slice is answered as a repeated start id is", func(t *testing.T) {
		r := &bundleRestore{results: map[string]shimwire.LauncherBundleResult{}}
		first := r.receive(frames[0])
		again := r.receive(frames[0])
		if again != first {
			t.Fatalf("the same slice sent twice answered %#v then %#v", first, again)
		}
		different := frames[0]
		different.Payload = append([]byte("not what was sent"), frames[0].Payload[17:]...)
		if got := r.receive(different); got.OK || !strings.Contains(got.Error, "reused an id with a different payload") {
			t.Fatalf("a different payload under a used id answered %#v", got)
		}
		if read, err := shimwire.ParseWorkspaceBundle(r.body); err == nil {
			t.Fatalf("a half-sent sequence left a body of %d bytes behind", len(read.Bundle))
		}
	})

	t.Run("a wrong digest is refused before anything is written", func(t *testing.T) {
		r := &bundleRestore{results: map[string]shimwire.LauncherBundleResult{}}
		corrupt := append([]shimwire.LauncherBundle{}, frames...)
		payload := append([]byte{}, corrupt[1].Payload...)
		payload[0] ^= 0xff
		corrupt[1].Payload = payload
		var last shimwire.LauncherBundleResult
		for _, frame := range corrupt {
			last = r.receive(frame)
		}
		if last.OK || last.Held || !strings.Contains(last.Error, "not the "+frames[0].SHA256) {
			t.Fatalf("a sequence that arrived changed answered %#v, want a refusal naming the digest it declared", last)
		}
		if r.body != nil {
			t.Fatalf("a refused sequence left %d bytes to restore", len(r.body))
		}
		if restored, notice := r.apply(context.Background(), []string{"LEGION_WORKSPACE=/nowhere"}); restored || notice != "" {
			t.Fatalf("a refused sequence restored something: restored=%v notice=%q", restored, notice)
		}
	})

	t.Run("a slice that disagrees with its sequence is refused", func(t *testing.T) {
		r := &bundleRestore{results: map[string]shimwire.LauncherBundleResult{}}
		if got := r.receive(frames[0]); !got.OK {
			t.Fatalf("the first slice answered %#v", got)
		}
		odd := frames[1]
		odd.SHA256 = strings.Repeat("0", 64)
		if got := r.receive(odd); got.OK || !strings.Contains(got.Error, "does not match its sequence") {
			t.Fatalf("a slice of another digest answered %#v", got)
		}
	})
}

// The launcher's command loop ends its connection on a frame type it does not know, so the bundle
// pair has to be in its switch: a launcher that killed the connection would take the pod's roles
// with it.
func TestTheLauncherAnswersABundleFrameOnItsConnection(t *testing.T) {
	g := newRig(t)
	saved := shimwire.WorkspaceBundle{Bundle: []byte("PACK not a real bundle"), Head: "f2f13454", At: time.Now().UTC()}
	frames, err := shimwire.SplitWorkspaceBundle("restore-2", saved)
	if err != nil {
		t.Fatal(err)
	}
	for _, frame := range frames {
		got, ok := g.answer(frame).(shimwire.LauncherBundleResult)
		if !ok || !got.OK {
			t.Fatalf("the launcher answered %#v", got)
		}
	}
	// The connection is still the one the daemon can start a generation on.
	marker := filepath.Join(t.TempDir(), "marker")
	if result := g.start(g.childStart(1, marker, nil)); !result.OK {
		t.Fatalf("after the bundle the start answered %#v", result)
	}
	g.stop(1)
}

// The whole of both halves against real jj and git: one pod's shim bundles the unpushed work of
// its workspace — a working-copy change, an unpushed commit on the issue's branch, an unpushed
// commit on a second head and a moved local bookmark — nothing reaches the remote, and the
// launcher of the pod that replaces it restores all four into a workspace of its own before any
// role starts.
func TestAReleasedWorkspacesWorkIsBundledByTheShimAndRestoredByTheLauncher(t *testing.T) {
	testbin.RequireJJFilters(t)
	f := newBundleFixture(t)
	f.workOne()
	saved := f.bundleWorkspaceOne()

	if remote := f.run(f.remote, "git", "--git-dir="+f.remote, "log", "--all", "--oneline"); strings.Contains(remote, "working copy") ||
		strings.Contains(remote, "second head") || strings.Contains(remote, "issue work") {
		t.Fatalf("the save pushed to the remote, which now holds:\n%s", remote)
	}
	if len(saved.Tips) < 3 {
		t.Fatalf("the save kept %d heads, want the working copy, the issue branch's and the second head: %v", len(saved.Tips), saved.Tips)
	}
	if saved.Bookmarks["legion/"+bundleIssue] != f.commits["issue work"] || saved.Bookmarks["side"] != f.commits["second head"] {
		t.Fatalf("the save kept the bookmarks %v, want legion/%s on %s and side on %s",
			saved.Bookmarks, bundleIssue, f.commits["issue work"], f.commits["second head"])
	}
	if _, kept := saved.Bookmarks["main"]; kept {
		t.Fatalf("the save kept main, which points at a commit the remote already holds: %v", saved.Bookmarks)
	}
	if saved.Head != f.commits["working copy"] {
		t.Fatalf("the save recorded the working copy as %s, want %s", saved.Head, f.commits["working copy"])
	}

	workspace := f.secondPod("ws2")
	notice := f.restoreInto(workspace, saved, 1)
	if !strings.Contains(notice, "restored") {
		t.Fatalf("the restore's notice is %q", notice)
	}
	log := f.run(workspace, "jj", "log", "-R", workspace, "--ignore-working-copy", "-r", "all()", "--no-graph",
		"-T", `commit_id ++ " " ++ description.first_line() ++ "\n"`)
	for _, want := range []string{"issue work", "second head", "working copy"} {
		if !strings.Contains(log, f.commits[want]+" "+want) {
			t.Fatalf("the restored workspace's log holds no %s (%s), so that head is not visible:\n%s", want, f.commits[want], log)
		}
	}
	listed := f.run(workspace, "jj", "bookmark", "list", "-R", workspace, "--ignore-working-copy",
		"-T", `if(remote, "", name ++ " " ++ normal_target.commit_id() ++ "\n")`)
	for name, commit := range map[string]string{"legion/" + bundleIssue: f.commits["issue work"], "side": f.commits["second head"]} {
		if !strings.Contains(listed, name+" "+commit) {
			t.Fatalf("the restored workspace has no %s on %s:\n%s", name, commit, listed)
		}
	}
	if head := f.run(workspace, "jj", "log", "-R", workspace, "--ignore-working-copy", "-r", "@", "--no-graph", "-T", "commit_id"); head != saved.Head {
		t.Fatalf("the restored working copy is on %s, want the saved %s", head, saved.Head)
	}
	if body, err := os.ReadFile(filepath.Join(workspace, "wip.txt")); err != nil || string(body) != "wip\n" {
		t.Fatalf("the restored working copy's own change reads %q (%v)", body, err)
	}
	// Nothing of the restore is left in the repository: the temporary branches are gone.
	if refs := f.run(workspace, "git", "--git-dir="+f.gitDir(workspace), "for-each-ref", "--format=%(refname)", "refs/heads/"); strings.Contains(refs, restoreRefPrefix) {
		t.Fatalf("the restore left its temporary branches behind:\n%s", refs)
	}
}

// A base the restore needs can be gone — a branch force-pushed away since the release — and the
// bundle is then unusable. The roles still start, in the workspace provisioning made, and the
// notice names what was lost and why.
func TestARestoreWhoseBaseCommitIsGoneStartsTheRolesWithANotice(t *testing.T) {
	testbin.RequireJJFilters(t)
	f := newBundleFixture(t)
	f.workOne()
	saved := f.bundleWorkspaceOne()

	// The remote's branches move back to the first commit, so the pod that replaces the released
	// one clones a repository that lacks the commit the bundle is built on.
	f.run(f.remote, "git", "--git-dir="+f.remote, "update-ref", "refs/heads/main", f.commits["one"])
	f.run(f.remote, "git", "--git-dir="+f.remote, "update-ref", "refs/heads/legion/"+bundleIssue, f.commits["one"])

	workspace := f.secondPod("ws3")
	notice := f.restoreInto(workspace, saved, 1)
	if !strings.Contains(notice, "not restored") || !strings.Contains(notice, "prerequisite") {
		t.Fatalf("the notice is %q, want it to name the work that was lost and the base git could not find", notice)
	}
	if !strings.Contains(notice, saved.At.UTC().Format(time.RFC3339)) {
		t.Fatalf("the notice is %q, want it to name when the lost work was saved", notice)
	}
	if log := f.run(workspace, "jj", "log", "-R", workspace, "--ignore-working-copy", "-r", "all()", "--no-graph", "-T", "commit_id ++ \"\\n\""); strings.Contains(log, saved.Head) {
		t.Fatalf("a restore that failed left the saved working copy in the workspace:\n%s", log)
	}
}

const bundleIssue = "INFRA-945"

// bundleFixture is one remote, the pod that is released and the workspace it worked in, and the
// pods that replace it: real git and real jj throughout, every command naming the directory it
// runs in (docs/testing.md).
type bundleFixture struct {
	t       *testing.T
	root    string
	remote  string
	clone   string
	ws      string
	env     []string
	commits map[string]string
}

func newBundleFixture(t *testing.T) *bundleFixture {
	t.Helper()
	for _, tool := range []string{"jj", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is required: %v", tool, err)
		}
	}
	root := t.TempDir()
	// The host's own jj and git configuration (its user, its trailers, its filters) must not
	// reach the fixture.
	config := filepath.Join(root, "jj.toml")
	if err := os.WriteFile(config, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f := &bundleFixture{
		t: t, root: root, remote: filepath.Join(root, "remote"), clone: filepath.Join(root, "clone"), ws: filepath.Join(root, "ws"),
		commits: map[string]string{},
		env: append(os.Environ(),
			"JJ_CONFIG="+config, "JJ_USER=tester", "JJ_EMAIL=tester@example.test",
			"GIT_AUTHOR_NAME=tester", "GIT_AUTHOR_EMAIL=tester@example.test",
			"GIT_COMMITTER_NAME=tester", "GIT_COMMITTER_EMAIL=tester@example.test",
			"GIT_CONFIG_GLOBAL="+filepath.Join(root, "gitconfig"), "GIT_CONFIG_SYSTEM="+filepath.Join(root, "gitconfig")),
	}
	seed := filepath.Join(root, "seed")
	for _, dir := range []string{f.remote, seed} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.run(f.remote, "git", "init", "--quiet", "--bare", f.remote)
	f.run(f.remote, "git", "--git-dir="+f.remote, "symbolic-ref", "HEAD", "refs/heads/main")
	f.run(seed, "git", "init", "--quiet", "-b", "main", seed)
	f.write(filepath.Join(seed, "f.txt"), "one\n")
	f.run(seed, "git", "add", "-A")
	f.run(seed, "git", "commit", "--quiet", "-m", "one")
	f.commits["one"] = f.run(seed, "git", "rev-parse", "HEAD")
	f.run(seed, "git", "remote", "add", "origin", f.remote)
	f.run(seed, "git", "push", "--quiet", "-u", "origin", "main")
	f.write(filepath.Join(seed, "f.txt"), "two\n")
	f.run(seed, "git", "commit", "--quiet", "-am", "two")
	f.run(seed, "git", "push", "--quiet", "origin", "main")
	f.run(seed, "git", "push", "--quiet", "origin", "main:refs/heads/legion/"+bundleIssue)
	f.pod(f.clone, f.ws)
	return f
}

// pod is a pod's shared clone and the issue workspace in it, as provisioning makes them: a
// colocated clone of the remote with every remote branch tracked, and one `jj workspace add`.
func (f *bundleFixture) pod(clone, workspace string) {
	f.t.Helper()
	// --no-local: a local clone hardlinks the whole object store, unreachable objects included,
	// where a pod clones over the network and gets only what the remote's refs reach.
	f.run(f.root, "git", "clone", "--quiet", "--no-local", f.remote, clone)
	f.run(clone, "jj", "git", "init", "--colocate", clone)
	f.run(clone, "jj", "bookmark", "track", "main@origin", "legion/"+bundleIssue+"@origin")
	f.run(clone, "jj", "workspace", "add", "--name", filepath.Base(workspace), workspace)
}

// secondPod is the pod that replaces the released one: its own clone of the remote as it stands
// now, and its own workspace, holding only what was pushed.
func (f *bundleFixture) secondPod(name string) string {
	f.t.Helper()
	clone, workspace := filepath.Join(f.root, name+"-clone"), filepath.Join(f.root, name)
	f.pod(clone, workspace)
	return workspace
}

// workOne leaves in the released pod's workspace the four kinds of unpushed work a release has to
// keep: a commit on the issue's branch, the working copy's own change on top of it, a commit on a
// second head, and a local bookmark moved onto that head.
func (f *bundleFixture) workOne() {
	f.t.Helper()
	f.run(f.ws, "jj", "new", "-R", f.ws, `remote_bookmarks(exact:"legion/`+bundleIssue+`")`, "-m", "issue work")
	f.write(filepath.Join(f.ws, "issue.txt"), "issue\n")
	f.run(f.ws, "jj", "bookmark", "set", "-R", f.ws, "--allow-backwards", "legion/"+bundleIssue, "-r", "@")
	f.commits["issue work"] = f.at()
	f.run(f.ws, "jj", "new", "-R", f.ws, "-m", "working copy")
	f.write(filepath.Join(f.ws, "wip.txt"), "wip\n")
	working := f.at()
	f.run(f.ws, "jj", "new", "-R", f.ws, "trunk()", "-m", "second head")
	f.write(filepath.Join(f.ws, "second.txt"), "second\n")
	f.run(f.ws, "jj", "bookmark", "create", "-R", f.ws, "side", "-r", "@")
	f.commits["second head"] = f.at()
	f.run(f.ws, "jj", "edit", "-R", f.ws, working)
	f.commits["working copy"] = f.at()
}

// at is the workspace's working-copy commit, after the snapshot the read itself makes.
func (f *bundleFixture) at() string {
	f.t.Helper()
	return f.run(f.ws, "jj", "log", "-R", f.ws, "-r", "@", "--no-graph", "-T", "commit_id")
}

// bundleWorkspaceOne runs the real shim against the released pod's workspace and returns the
// bundle it built: the daemon's end of the worker stream, one bundle-workspace frame, and the
// result the shim answers, reassembled from its chunk lane as the daemon's reader reassembles it.
func (f *bundleFixture) bundleWorkspaceOne() shimwire.WorkspaceBundle {
	f.t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = listener.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		// The wrapped process is a plain `cat`: the shim bridges its stdio and answers the
		// bundle frame itself, so nothing of Oh My Pi is needed to prove the save.
		_, _ = shim.Run(ctx, shim.Config{
			Network: "tcp", Address: listener.Addr().String(), BootToken: "boot",
			Argv: []string{"/bin/sh", "-c", "exec cat"},
			Env:  append(f.env, "LEGION_WORKSPACE="+f.ws),
		})
	}()
	f.t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(20 * time.Second):
			f.t.Error("the shim did not leave after its context ended")
		}
	})

	conn, err := listener.Accept()
	if err != nil {
		f.t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Minute)); err != nil {
		f.t.Fatal(err)
	}
	reader := shimwire.NewReader(bufio.NewReader(conn))
	if line, err := reader.ReadLine(); err != nil {
		f.t.Fatalf("the shim's hello: %v", err)
	} else if frame, err := shimwire.Decode(line); err != nil || frame.FrameType() != shimwire.TypeHello2 {
		f.t.Fatalf("the shim opened with %v (%v)", frame, err)
	}
	writer := shimwire.NewWriter(conn)
	if err := writer.WriteFrame(shimwire.HelloAck{}); err != nil {
		f.t.Fatal(err)
	}
	request := shimwire.BundleWorkspace{ID: "bundle-1", Issue: bundleIssue, MaxBytes: 40 << 20, TimeoutMs: 120_000}
	if err := writer.WriteFrame(request); err != nil {
		f.t.Fatal(err)
	}
	var chunks shimwire.Reassembler
	for {
		line, err := reader.ReadLine()
		if err != nil {
			f.t.Fatalf("read the shim's answer: %v", err)
		}
		frame, err := shimwire.Decode(line)
		if err != nil {
			continue
		}
		if chunk, ok := frame.(shimwire.RPCChunk); ok {
			whole, err := chunks.Push(chunk)
			if err != nil {
				f.t.Fatalf("reassemble the shim's answer: %v", err)
			}
			if whole == nil {
				continue
			}
			if frame, err = shimwire.Decode(whole); err != nil {
				f.t.Fatalf("decode the shim's reassembled answer: %v", err)
			}
		}
		result, ok := frame.(shimwire.BundleWorkspaceResult)
		if !ok {
			continue
		}
		if !result.OK {
			f.t.Fatalf("the shim refused the save: %s", result.Error)
		}
		if err := result.Validate(); err != nil {
			f.t.Fatal(err)
		}
		return shimwire.WorkspaceBundle{
			Bundle: result.Bundle, Head: result.Head, Tips: result.Tips, Bookmarks: result.Bookmarks, At: result.At,
		}
	}
}

// restoreInto drives one replacement pod's launcher: the bundle in launcher_bundle slices, then
// the generation's start, which applies the restore before its child runs. It returns the notice
// the launcher wrote for the restore and stops the generation.
func (f *bundleFixture) restoreInto(workspace string, saved shimwire.WorkspaceBundle, generation uint64) string {
	f.t.Helper()
	g := newRig(f.t)
	frames, err := shimwire.SplitWorkspaceBundle("restore-"+workspace, saved)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, frame := range frames {
		got, ok := g.answer(frame).(shimwire.LauncherBundleResult)
		if !ok || !got.OK {
			f.t.Fatalf("slice %d answered %#v", frame.Index, got)
		}
		if want := frame.Index == len(frames)-1; got.Held != want {
			f.t.Fatalf("slice %d answered held=%v, want %v", frame.Index, got.Held, want)
		}
	}
	marker := filepath.Join(f.t.TempDir(), "marker")
	command := g.childStart(generation, marker, nil)
	command.Env = append(command.Env, append(f.env, "LEGION_WORKSPACE="+workspace)...)
	if result := g.start(command); !result.OK {
		f.t.Fatalf("the start after the bundle answered %#v", result)
	}
	published(f.t, marker)
	g.stop(generation)
	for _, line := range strings.Split(g.stderr.String(), "\n") {
		if strings.Contains(line, "released workspace's saved work") {
			return line
		}
	}
	f.t.Fatalf("the launcher wrote no notice for the restore:\n%s", g.stderr.String())
	return ""
}

// gitDir is the git directory behind a workspace, as the restore resolves it.
func (f *bundleFixture) gitDir(workspace string) string {
	f.t.Helper()
	dir, err := workspaceGitDir(workspace)
	if err != nil {
		f.t.Fatal(err)
	}
	return dir
}

func (f *bundleFixture) write(path, content string) {
	f.t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// run runs one jj or git command in dir, which is the directory it reads its repository from: jj
// searches upward from the directory it runs in for the closest .jj, so a command with no Dir
// would open the workspace the test process stands in (docs/testing.md).
func (f *bundleFixture) run(dir string, args ...string) string {
	f.t.Helper()
	command := exec.Command(args[0], args[1:]...)
	command.Dir = dir
	command.Env = f.env
	output, err := command.CombinedOutput()
	if err != nil {
		f.t.Fatalf("%s in %s: %v\n%s", strings.Join(args, " "), dir, err, output)
	}
	return strings.TrimSpace(string(output))
}
