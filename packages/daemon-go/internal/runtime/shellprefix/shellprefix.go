// Package shellprefix builds PI_SHELL_PREFIX, the shell command Oh My Pi's bash tool runs before
// each of the agent's commands (`<prefix> <command>`, in its persistent shell). Every runtime
// hands its agents one, over the directories that process's own `gh` and `legion` live in: a tmux
// pane's under the daemon's state directory, a pod's in the worker image.
package shellprefix

import (
	"path/filepath"
	"strings"
)

// For is the prefix that makes dirs the first entries of PATH, in order. The agent's shell has
// sourced the operator's rc file and replays the PATH the rc left, so an rc that prepends its own
// directories puts them ahead of the runtime's: on the devbox the dotfiles shims, whose gh is not
// Legion's. The prefix moves dirs back to the front, removing the copy an earlier command put
// there, so the agent's plain gh and legion are the runtime's and PATH stops growing. PATH is
// already exported; the assignment ends in `&&`, never `;`, because tmux splits its argv at an
// argument ending in one.
func For(dirs ...string) string {
	separator := string(filepath.ListSeparator)
	head := strings.Join(dirs, separator) + separator
	return "PATH=" + literal(head) + "${PATH#" + literal(head) + "} &&"
}

// literal is value as one single-quoted shell word, each `'` closed, escaped, and reopened.
func literal(value string) string { return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'" }
