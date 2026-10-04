// Package shellprefix builds PI_SHELL_PREFIX, the shell command Oh My Pi's bash tool runs before
// each of the agent's commands (`<prefix> <command>`, in its persistent shell). Every runtime
// hands its agents one, over the directories that process's own `gh` and `legion` live in: a tmux
// pane's under the daemon's state directory, a pod's in the worker image. Its quoting, Literal, is
// also what the scripts the workerbin package installs quote with, and Word and Command are the
// daemon's one rendering of an argument, and of an argv, as shell text.
package shellprefix

import (
	"path/filepath"
	"regexp"
	"strings"
)

// For is the prefix that makes dirs the first entries of PATH, in order, each exactly once. The
// agent's shell has sourced the operator's rc file and replays the PATH the rc left, so an rc that
// prepends its own directories puts them ahead of the runtime's: on the devbox the dotfiles shims,
// whose gh is not Legion's. The prefix removes every entry that is one of dirs, wherever the rc
// left it, keeps every other entry in its order, empty ones included, and puts dirs in front, so
// the agent's plain gh and legion are the runtime's and PATH never grows. It works on PATH with
// each entry wrapped in its own pair of separators (`:a::b:`), where an entry equal to a dir is
// exactly one `:dir:` match, in a variable it unsets again. It ends with `hash -r`: Oh My Pi's
// bash tool runs in brush, which keeps a command it has looked up after PATH changes where bash
// forgets it, so one earlier `PATH=<dir>:$PATH gh` would otherwise leave every later plain gh
// running that dir's. PATH is already exported; every step ends in `&&`, never `;`, because tmux
// splits its argv at an argument ending in one.
func For(dirs ...string) string {
	const sep, rest = string(filepath.ListSeparator), "__legion_path"
	steps := []string{rest + "=" + sep + "${PATH//" + sep + "/" + sep + sep + "}" + sep}
	for _, dir := range dirs {
		steps = append(steps, rest+"=${"+rest+"//"+sep+Literal(dir)+sep+"/}")
	}
	steps = append(steps,
		rest+"=${"+rest+"//"+sep+sep+"/"+sep+"}",
		rest+"=${"+rest+"#"+sep+"}",
		"PATH="+Literal(strings.Join(dirs, sep))+"${"+rest+":+"+sep+"${"+rest+"%"+sep+"}}",
		"unset "+rest,
		"hash -r",
	)
	return strings.Join(steps, " && ") + " &&"
}

// Literal is value as one single-quoted shell word, each `'` closed, escaped, and reopened.
func Literal(value string) string { return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'" }

// shellUnsafe is any character outside the set a POSIX shell reads literally in a bare word.
var shellUnsafe = regexp.MustCompile(`[^A-Za-z0-9_./:-]`)

// Word renders value as one shell word: bare when every character is literal, else Literal
// (shellPath, runtime.ts). Word("") returns "", zero words, so an empty argument does not survive
// it.
func Word(value string) string {
	if !shellUnsafe.MatchString(value) {
		return value
	}
	return Literal(value)
}

// Command renders argv as one shell command line: each element one Word, joined by spaces.
func Command(argv []string) string {
	words := make([]string, len(argv))
	for i, arg := range argv {
		words[i] = Word(arg)
	}
	return strings.Join(words, " ")
}
