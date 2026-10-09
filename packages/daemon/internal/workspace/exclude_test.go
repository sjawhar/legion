package workspace

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCleanExcludeReadsEachPathOnceInsideTheRepository(t *testing.T) {
	got, err := CleanExclude([]string{"tasks/", "src/gen", "docs[1]"})
	if err != nil {
		t.Fatalf("CleanExclude: %v", err)
	}
	if want := []string{"tasks", "src/gen", "docs[1]"}; !slices.Equal(got, want) {
		t.Fatalf("CleanExclude = %q, want %q", got, want)
	}
	for _, tc := range []struct {
		paths []string
		want  string
	}{
		{[]string{""}, `entry "" must be a path inside the repository`},
		{[]string{"/"}, `entry "/" must be a path inside the repository`},
		{[]string{"/tasks"}, `entry "/tasks" must be a path inside the repository`},
		{[]string{"a//b"}, `entry "a//b" must be a path inside the repository`},
		{[]string{"./tasks"}, `entry "./tasks" must be a path inside the repository`},
		{[]string{"tasks/.."}, `entry "tasks/.." must be a path inside the repository`},
		{[]string{".legion"}, `entry ".legion" names .legion, where every role writes its handoffs`},
		{[]string{".legion/LEGION-1/"}, `entry ".legion/LEGION-1/" names .legion, where every role writes its handoffs`},
		{[]string{"tasks", "tasks/"}, `names "tasks" twice`},
		{[]string{"tasks/t1", "tasks"}, `entry "tasks/t1" is inside "tasks", which the list already leaves out`},
	} {
		if _, err := CleanExclude(tc.paths); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("CleanExclude(%q) = %v, want an error containing %q", tc.paths, err, tc.want)
		}
	}
}

// pushTree makes run's remote main one commit holding files (path -> contents) on top of what it
// had, as github.com/acme/widgets would.
func pushTree(t *testing.T, run *recordingRunner, files map[string]string) {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	identity := []string{
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Legion test", "GIT_AUTHOR_EMAIL=legion-test@example.invalid",
		"GIT_COMMITTER_NAME=Legion test", "GIT_COMMITTER_EMAIL=legion-test@example.invalid",
	}
	runSetupWith(t, root, identity, "git", "clone", "--quiet", run.remote, work)
	for path, contents := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(work, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, path), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runSetupWith(t, work, identity, "git", "add", "--all")
	runSetupWith(t, work, identity, "git", "commit", "--quiet", "-m", "tree")
	runSetupWith(t, work, identity, "git", "push", "--quiet", "origin", "HEAD:main")
}

// checkedOut is every file in dir's working copy, relative to dir, skipping jj's and git's own.
func checkedOut(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && (entry.Name() == ".jj" || entry.Name() == ".git") {
			return filepath.SkipDir
		}
		if entry.Name() == ".git" {
			return nil // a git worktree's pointer file
		}
		if !entry.IsDir() {
			relative, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(relative))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(files)
	return files
}

var excludeFixture = map[string]string{
	"README.md":                 "widgets\n",
	"src/main.go":               "package main\n",
	"src/gen/out.txt":           "generated\n",
	"tasks/t1/basic_info.json":  "{}\n",
	"tasks/t2/data.txt":         "data\n",
	".legion/WIDGETS-42/a.json": "{}\n",
}

// A new workspace leaves out the paths its request names: none of their files is ever written,
// every other path at its starting commit is checked out, jj sees no change, and a nested path
// keeps its directory's other entries. Another issue's workspace on the same clone with no
// exclusions checks out everything.
func TestProvisionLeavesTheExcludedPathsOutOfANewWorkspace(t *testing.T) {
	run := newLocalRunner(t)
	pushTree(t, run, excludeFixture)
	request := provisionRequest(t)
	request.Exclude = []string{"tasks/", "src/gen"}
	workspace, err := Provision(context.Background(), run, request)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if got, want := checkedOut(t, workspace.Dir), []string{".legion/WIDGETS-42/a.json", "README.md", "src/main.go"}; !slices.Equal(got, want) {
		t.Fatalf("the workspace checks out %q, want %q", got, want)
	}
	for _, path := range []string{"tasks", "src/gen"} {
		if _, err := os.Stat(filepath.Join(workspace.Dir, path)); !os.IsNotExist(err) {
			t.Fatalf("%s in the workspace: %v, want it absent", path, err)
		}
	}
	if patterns := runSetup(t, workspace.Dir, "jj", "sparse", "list"); patterns != ".legion\nREADME.md\nsrc/main.go\n" {
		t.Fatalf("jj sparse list = %q, want every entry but the excluded paths and their directories", patterns)
	}
	if status := runSetup(t, workspace.Dir, "jj", "status"); !strings.Contains(status, "The working copy has no changes") {
		t.Fatalf("jj status = %q, want no change: a path left out is not a deletion", status)
	}
	if parent := runSetup(t, workspace.Dir, "jj", "log", "-r", "@-", "--no-graph", "-T", "description.first_line()"); parent != "tree" {
		t.Fatalf("the workspace sits on %q, want the remote's main", parent)
	}

	full := request
	full.Issue, full.Exclude = "WIDGETS-91", nil
	other, err := Provision(context.Background(), run, full)
	if err != nil {
		t.Fatalf("provision an issue with no exclusions: %v", err)
	}
	if got := checkedOut(t, other.Dir); len(got) != len(excludeFixture) {
		t.Fatalf("a workspace with no exclusions checks out %q, want every file", got)
	}
}

// pushIssueBranchWithoutHandoffs pushes legion/WIDGETS-42 to run's remote as main with its
// `.legion/` deleted, the commit ghbranch.Create starts an issue branch on.
func pushIssueBranchWithoutHandoffs(t *testing.T, run *recordingRunner) {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	identity := []string{
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Legion test", "GIT_AUTHOR_EMAIL=legion-test@example.invalid",
		"GIT_COMMITTER_NAME=Legion test", "GIT_COMMITTER_EMAIL=legion-test@example.invalid",
	}
	runSetupWith(t, filepath.Dir(work), identity, "git", "clone", "--quiet", run.remote, work)
	runSetupWith(t, work, identity, "git", "rm", "-r", "--quiet", handoffDir)
	runSetupWith(t, work, identity, "git", "commit", "--quiet", "-m", "strip .legion")
	runSetupWith(t, work, identity, "git", "push", "--quiet", "origin", "HEAD:refs/heads/legion/WIDGETS-42")
}

// An issue branch starts without `.legion/`, and the patterns still name it: a handoff written
// there is part of the working copy's change, as on a workspace that checks out everything.
func TestProvisionRecordsAHandoffOnAnIssueBranchWithoutHandoffs(t *testing.T) {
	run := newLocalRunner(t)
	pushTree(t, run, map[string]string{
		"README.md":                 "widgets\n",
		"tasks/t1/x.json":           "{}\n",
		".legion/OTHER-1/plan.json": "{}\n",
	})
	pushIssueBranchWithoutHandoffs(t, run)
	request := provisionRequest(t)
	request.Exclude = []string{"tasks"}
	workspace, err := Provision(context.Background(), run, request)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if parent := runSetup(t, workspace.Dir, "jj", "log", "-r", "@-", "--no-graph", "-T", "description.first_line()"); parent != "strip .legion" {
		t.Fatalf("the workspace sits on %q, want the issue branch", parent)
	}
	if patterns := runSetup(t, workspace.Dir, "jj", "sparse", "list"); patterns != ".legion\nREADME.md\n" {
		t.Fatalf("jj sparse list = %q, want .legion named though the branch lacks it", patterns)
	}
	handoff := filepath.Join(workspace.Dir, handoffDir, "WIDGETS-42", "plan.json")
	if err := os.MkdirAll(filepath.Dir(handoff), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(handoff, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if status := runSetup(t, workspace.Dir, "jj", "status"); !strings.Contains(status, "A .legion/WIDGETS-42/plan.json") {
		t.Fatalf("jj status after writing a handoff = %q, want it added", status)
	}
}

// An exclusion that names nothing in the starting commit leaves the workspace whole: it checks out
// everything, `.`, rather than going sparse for nothing.
func TestProvisionLeavesAWorkspaceWholeWhenNoExclusionMatches(t *testing.T) {
	run := newLocalRunner(t)
	pushTree(t, run, excludeFixture)
	request := provisionRequest(t)
	request.Exclude = []string{"fixtures", "README.md/inside-a-file"}
	workspace, err := Provision(context.Background(), run, request)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if patterns := runSetup(t, workspace.Dir, "jj", "sparse", "list"); patterns != ".\n" {
		t.Fatalf("jj sparse list = %q, want the whole repository", patterns)
	}
	if got := checkedOut(t, workspace.Dir); len(got) != len(excludeFixture) {
		t.Fatalf("the workspace checks out %q, want every file", got)
	}
}

// An excluded path the starting commit lacks splits no directory, even when another excluded path
// makes the workspace sparse: `src` stays one pattern, so a file a worker adds there is recorded.
func TestProvisionSplitsOnlyDirectoriesAboveAnExcludedPathTheCommitHas(t *testing.T) {
	run := newLocalRunner(t)
	pushTree(t, run, map[string]string{
		"README.md":       "widgets\n",
		"src/main.go":     "package main\n",
		"tasks/t1/x.json": "{}\n",
	})
	request := provisionRequest(t)
	request.Exclude = []string{"tasks", "src/gen"}
	workspace, err := Provision(context.Background(), run, request)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if patterns := runSetup(t, workspace.Dir, "jj", "sparse", "list"); patterns != ".legion\nREADME.md\nsrc\n" {
		t.Fatalf("jj sparse list = %q, want src whole: src/gen is not in the commit", patterns)
	}
	if err := os.WriteFile(filepath.Join(workspace.Dir, "src", "new.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if status := runSetup(t, workspace.Dir, "jj", "status"); !strings.Contains(status, "A src/new.go") {
		t.Fatalf("jj status after adding src/new.go = %q, want it added", status)
	}
}

// Exclusions shape only the workspace a provisioning creates: one that exists keeps its patterns,
// so a path its agent added back stays checked out on every later provisioning.
func TestProvisionKeepsAnExistingWorkspacesSparsePatterns(t *testing.T) {
	run := newLocalRunner(t)
	pushTree(t, run, excludeFixture)
	request := provisionRequest(t)
	request.Exclude = []string{"tasks"}
	workspace, err := Provision(context.Background(), run, request)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	runSetup(t, workspace.Dir, "jj", "sparse", "set", "--add", "tasks")
	if _, err := Provision(context.Background(), run, request); err != nil {
		t.Fatalf("provision again: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace.Dir, "tasks", "t2", "data.txt")); err != nil {
		t.Fatalf("tasks after a second provisioning: %v, want the agent's own patterns kept", err)
	}
}

// A workspace whose patterns could not be set is removed, never left an empty checkout the next
// provisioning would keep: the next provisioning creates it again, sparse.
func TestProvisionRemovesAWorkspaceWhoseSparsePatternsFailed(t *testing.T) {
	run := newLocalRunner(t)
	pushTree(t, run, excludeFixture)
	request := provisionRequest(t)
	request.Exclude = []string{"tasks"}
	run.failSparseSet = true
	_, err := Provision(context.Background(), run, request)
	if err == nil || !strings.Contains(err.Error(), "forced sparse set failure") {
		t.Fatalf("provision with a failing jj sparse set = %v, want that failure", err)
	}
	located, err := Location(request.StateDir, request.Repo, request.Issue)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(located.Dir); !os.IsNotExist(err) {
		t.Fatalf("the workspace directory after the failure: %v, want it removed", err)
	}
	if listed := runSetup(t, located.Clone, "jj", "workspace", "list", "--ignore-working-copy"); strings.Contains(listed, "widgets-42") {
		t.Fatalf("jj workspace list = %q, want the failed workspace forgotten", listed)
	}

	run.failSparseSet = false
	workspace, err := Provision(context.Background(), run, request)
	if err != nil {
		t.Fatalf("provision again: %v", err)
	}
	if got, want := checkedOut(t, workspace.Dir), []string{".legion/WIDGETS-42/a.json", "README.md", "src/gen/out.txt", "src/main.go"}; !slices.Equal(got, want) {
		t.Fatalf("the recreated workspace checks out %q, want %q", got, want)
	}
}

// Provisioning refuses an exclusion list CleanExclude refuses before it touches the volume.
func TestProvisionRefusesAnExclusionOutsideTheRepository(t *testing.T) {
	run := newLocalRunner(t)
	request := provisionRequest(t)
	request.Exclude = []string{"../escape"}
	_, err := Provision(context.Background(), run, request)
	if err == nil || !strings.Contains(err.Error(), `workspace exclusions: entry "../escape" must be a path inside the repository`) {
		t.Fatalf("provision = %v, want the exclusion refused", err)
	}
	if calls := run.Calls(); len(calls) != 0 {
		t.Fatalf("provisioning ran %q before refusing", calls[0].Argv)
	}
	if _, err := os.Stat(request.StateDir); !os.IsNotExist(err) {
		t.Fatalf("the state directory after the refusal: %v, want it untouched", err)
	}
}
