package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// handoffDir is where every role writes its handoffs in an issue workspace (`.legion/<issue>/`).
// A workspace never leaves it out: jj does not snapshot a file outside the sparse patterns, so a
// handoff written there would silently never reach the issue branch.
const handoffDir = ".legion"

// CleanExclude reads the paths a new issue workspace leaves out (the project's
// `workspace_exclude`, workspace-init provision's --exclude) into the form sparseInclude takes:
// each a path inside the repository, slash-separated, without its trailing slash, named once. It
// refuses the repository root, an absolute path, an empty, `.` or `..` segment, the handoff
// directory and anything inside it, and a path inside another one the list already names, whose
// own entry would leave nothing more out. The error names the entry; the caller names the list.
func CleanExclude(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	cleaned := make([]string, 0, len(paths))
	for _, raw := range paths {
		path := strings.TrimSuffix(raw, "/")
		if path == "" || strings.HasPrefix(path, "/") || slices.ContainsFunc(strings.Split(path, "/"), func(segment string) bool {
			return segment == "" || segment == "." || segment == ".."
		}) {
			return nil, fmt.Errorf("entry %q must be a path inside the repository: not its root, no leading /, and no empty, . or .. segment", raw)
		}
		if path == handoffDir || strings.HasPrefix(path, handoffDir+"/") {
			return nil, fmt.Errorf("entry %q names %s, where every role writes its handoffs; a workspace always checks it out", raw, handoffDir)
		}
		if slices.Contains(cleaned, path) {
			return nil, fmt.Errorf("names %q twice", path)
		}
		cleaned = append(cleaned, path)
	}
	for _, path := range cleaned {
		for _, other := range cleaned {
			if strings.HasPrefix(path, other+"/") {
				return nil, fmt.Errorf("entry %q is inside %q, which the list already leaves out", path, other)
			}
		}
	}
	return cleaned, nil
}

// sparseInclude is the sparse patterns that check out everything in revision but the paths
// exclude names (CleanExclude's form): jj's patterns name only what a working copy includes, each
// a path and everything below it, so a path is left out by naming every entry of each directory
// above it but the one on its way down. The entries come from the commit's own tree in the shared
// clone. The patterns always name the handoff directory, which an issue branch starts without
// (ghbranch.Create cuts it from main with `.legion/` deleted): jj records no file outside them.
//
// It returns no patterns when no excluded path is in revision's tree (a path the revision lacks,
// or one below a file), so the workspace checks out everything rather than going sparse for
// nothing.
//
// The patterns are computed once, for the revision the workspace is created at: an entry a later
// commit adds beside an excluded path is not checked out until the workspace's agent adds it
// (`jj sparse set --add <path>`), and nothing outside the patterns is ever changed in a commit.
func sparseInclude(ctx context.Context, run Runner, cloneDir, revision string, exclude []string) ([]string, error) {
	excluded := map[string]bool{}
	// Every directory above an excluded path, the repository root ("") included.
	holding := map[string]bool{"": true}
	for _, path := range exclude {
		excluded[path] = true
		for dir := path; strings.Contains(dir, "/"); {
			dir = dir[:strings.LastIndex(dir, "/")]
			holding[dir] = true
		}
	}
	dirs := make([]string, 0, len(holding))
	for dir := range holding {
		dirs = append(dirs, dir)
	}
	slices.Sort(dirs)
	var include []string
	leftOut := false
	for _, dir := range dirs {
		entries, err := treeEntries(ctx, run, cloneDir, revision, dir)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if excluded[entry.path] {
				leftOut = true
				continue
			}
			if entry.tree && holding[entry.path] {
				continue
			}
			include = append(include, entry.path)
		}
	}
	if !leftOut {
		return nil, nil
	}
	if !slices.Contains(include, handoffDir) {
		include = append(include, handoffDir)
	}
	slices.Sort(include)
	return include, nil
}

// treeEntry is one entry of a directory in a commit's tree: its path from the repository root,
// and whether it is a directory (a tree; a file, a symlink and a submodule are not).
type treeEntry struct {
	path string
	tree bool
}

// treeEntries lists dir's entries in revision's tree, read from the shared clone's git store with
// `git ls-tree`: the root's for "", none for a directory the revision does not have. Pathspecs are
// literal, so a directory named with a glob character lists that directory alone.
func treeEntries(ctx context.Context, run Runner, cloneDir, revision, dir string) ([]treeEntry, error) {
	argv := []string{"git", "--literal-pathspecs", "--git-dir=" + cloneDir + "/.git", "ls-tree", "-z", "--full-tree", revision}
	if dir != "" {
		argv = append(argv, "--", dir+"/")
	}
	listed, err := RunChecked(ctx, run, argv, nil, "")
	if err != nil {
		return nil, err
	}
	var entries []treeEntry
	for _, record := range bytes.Split([]byte(listed.Stdout), []byte{0}) {
		if len(record) == 0 {
			continue
		}
		// `<mode> SP <type> SP <object> TAB <path>`.
		meta, path, ok := bytes.Cut(record, []byte{'\t'})
		fields := strings.Fields(string(meta))
		if !ok || len(fields) != 3 || len(path) == 0 {
			return nil, fmt.Errorf("%s printed %q, not a tree entry", strings.Join(argv, " "), record)
		}
		entries = append(entries, treeEntry{path: string(path), tree: fields[1] == "tree"})
	}
	return entries, nil
}

// applySparse checks out include alone in the workspace jj just added with nothing in it
// (`--sparse-patterns empty`). A workspace whose patterns could not be set is removed, so the next
// provisioning, which adds only a workspace it finds no directory for, creates it again rather
// than leaving an issue's agents an empty checkout; it holds no work yet.
func applySparse(ctx context.Context, run Runner, workspace Workspace, include []string) error {
	set := []string{"jj", "sparse", "set", "--clear"}
	for _, path := range include {
		set = append(set, "--add", path)
	}
	_, err := RunCheckedIn(ctx, run, workspace, set)
	if err == nil {
		return nil
	}
	if removeErr := Remove(ctx, run, workspace); removeErr != nil {
		return errors.Join(err, fmt.Errorf("remove workspace %s, whose sparse patterns were not set: %w", workspace.Dir, removeErr))
	}
	return err
}
