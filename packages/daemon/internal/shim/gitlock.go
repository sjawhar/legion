package shim

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

// gitLockReports are the ways a git or jj failure names the lock file it could not take: jj's git
// backend (gitoxide), and git's own lockfile API, which every git jj runs reports through.
var gitLockReports = []*regexp.Regexp{
	regexp.MustCompile(`The lockfile at '([^']+)' might need manual deletion`),
	regexp.MustCompile(`Unable to create '([^']+)': File exists`),
}

// staleGitLockNote is one line for each git lock file output names that is still on disk: its
// path, how long ago it was last written, and what becomes of it. A git or jj interrupted mid-write
// leaves its lock, and every later write of what it locks fails on it. In a pod no process removes
// it while the pod runs, since another role's git, in a container whose processes this one cannot
// see, may hold it; replacing the issue's pod does (`workspace-init provision`,
// cmd/legion/workspace_init.go). Under tmux nothing removes it. The Legion tools' staleGitLockNote
// (packages/pi-shared/src/git-lock.ts) words the same note for its own jj calls. Empty when
// output names no lock, or each one it names is already gone.
func staleGitLockNote(output string, now time.Time) string {
	var locks, notes []string
	for _, report := range gitLockReports {
		for _, match := range report.FindAllStringSubmatch(output, -1) {
			if !slices.Contains(locks, match[1]) {
				locks = append(locks, match[1])
			}
		}
	}
	for _, lock := range locks {
		info, err := os.Lstat(lock)
		if err != nil {
			continue
		}
		notes = append(notes, fmt.Sprintf("git lock %s was last written %s ago and blocks this command. "+
			"Do not remove it while the issue's pod runs: another role's git, which this container cannot see, may hold it. "+
			"Replacing the issue's pod removes it. "+
			"Under the tmux runtime nothing removes it: remove it by hand only once no git or jj process on the host is using the repository.",
			lock, now.Sub(info.ModTime()).Round(time.Second)))
	}
	return strings.Join(notes, "\n")
}
