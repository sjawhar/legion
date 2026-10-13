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
// leaves its lock, and every later write of what it locks fails on it; under a running pod no
// process removes it, since a live git may still hold it, and the issue pod's next start does
// (`workspace-init provision`, cmd/legion/workspace_init.go). pi-legion's staleGitLockNote
// (packages/pi-legion/src/git-lock.ts) words the same note for its own jj calls. Empty when
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
			"If no git or jj process is still running, an interrupted one left it. "+
			"Legion never removes a lock under a running pod; on Kubernetes the issue pod's next start removes it before any role runs.",
			lock, now.Sub(info.ModTime()).Round(time.Second)))
	}
	return strings.Join(notes, "\n")
}
