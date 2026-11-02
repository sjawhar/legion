package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The plumbing a release's two halves share: the shim's save of an issue workspace's unpushed work
// (internal/shim) and the launcher's restore of it into the pod that replaces the released one
// (internal/runtime/sandbox's launcher is the daemon's side; internal/launcher runs in the pod).
// Both read the same jj workspace with the same git, so one copy of each answer keeps them from
// drifting apart: a save that found the git directory one way and a restore that found it another
// would put the work back somewhere the save never took it from.

// GitDir is the git directory behind a jj workspace, read from jj's own files rather than from a
// git that would search upward from wherever it was started: <workspace>/.jj/repo names the
// repository (a directory in the repository's own workspace, a file holding its path in every
// other), and <repo>/store/git_target names the git directory relative to <repo>/store. Each
// caller wraps the error with the prefix its own component reports under.
func GitDir(workspace string) (string, error) {
	repo := filepath.Join(workspace, ".jj", "repo")
	info, err := os.Stat(repo)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", repo, err)
	}
	if !info.IsDir() {
		named, err := os.ReadFile(repo)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", repo, err)
		}
		repo = absoluteUnder(filepath.Join(workspace, ".jj"), strings.TrimSpace(string(named)))
	}
	target, err := os.ReadFile(filepath.Join(repo, "store", "git_target"))
	if err != nil {
		return "", fmt.Errorf("read the workspace's git directory: %w", err)
	}
	return absoluteUnder(filepath.Join(repo, "store"), strings.TrimSpace(string(target))), nil
}

// absoluteUnder resolves path as jj writes it: absolute as it stands, relative to base otherwise.
func absoluteUnder(base, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(base, path))
}

// RefUpdates is the `git update-ref --stdin` input that creates or deletes one temporary ref per
// saved head, named prefix followed by the head's place in tips, in the order the heads were read.
// verb is `create` or `delete`; a delete names no commit. The save and the restore each pass the
// prefix their own refs live under, which is the only difference between them.
func RefUpdates(verb, prefix string, tips []string) []byte {
	var out strings.Builder
	for index, tip := range tips {
		out.WriteString(verb + " " + prefix + strconv.Itoa(index))
		if verb == "create" {
			out.WriteString(" " + tip)
		}
		out.WriteString("\n")
	}
	return []byte(out.String())
}

// OutputLines is out's non-empty lines, trimmed: what a jj or git command's output is read as,
// and what a multi-line report is joined back from for a notice that must be one line.
func OutputLines(out string) []string {
	var kept []string
	for line := range strings.SplitSeq(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			kept = append(kept, line)
		}
	}
	return kept
}
