package tmux

import (
	"regexp"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// shellUnsafe is any character outside the set a POSIX shell reads literally in a bare word.
var shellUnsafe = regexp.MustCompile(`[^A-Za-z0-9_./:-]`)

// shellPath renders value as one shell word: bare when every character is literal, else
// single-quoted with each `'` closed, escaped, and reopened (runtime.ts:327-329).
func shellPath(value string) string {
	if !shellUnsafe.MatchString(value) {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// doubleQuoteEscaper escapes the four characters a shell still interprets inside double quotes.
var doubleQuoteEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "`", "\\`")

// systemPromptArgument is the one `--append-system-prompt` word every pane's OMP receives. OMP's
// flag is last-wins, so every fragment rides a single value, in order: the role prompt files, the
// addressing text, then the deployment instructions file, separated by a blank line. It is one
// double-quoted word holding `$(cat <files>)`, so the pane's own shell reads the files — their
// size and quoting never pass through tmux's argv — and the addressing text is escaped for the
// double quotes (runtime-tmux.ts:79-103). The caller has checked there is a role prompt.
func systemPromptArgument(parts runtime.PromptParts) string {
	quoted := make([]string, len(parts.RolePromptPaths))
	for i, path := range parts.RolePromptPaths {
		quoted[i] = shellPath(path)
	}
	fragments := []string{"$(cat " + strings.Join(quoted, " ") + ")"}
	if parts.Addressing != "" {
		fragments = append(fragments, doubleQuoteEscaper.Replace(parts.Addressing))
	}
	if parts.DeploymentInstructionsPath != "" {
		fragments = append(fragments, "$(cat "+shellPath(parts.DeploymentInstructionsPath)+")")
	}
	return `--append-system-prompt "` + strings.Join(fragments, "\n\n") + `"`
}

// WithOmpLaunchPrefix prepends the configured launch prefix, each element quoted on its own, to
// the OMP invocation — a fragment already fit for the shell. The prefix is how a provider key is
// obtained inside the pane (`secrets <KEY> --`) without the daemon holding it
// (runtime-tmux.ts:105-118). Every pane runs it, and so does the daemon's boot gate, which must
// launch Oh My Pi exactly as a pane will.
func WithOmpLaunchPrefix(prefix []string, invocation string) string {
	if len(prefix) == 0 {
		return invocation
	}
	quoted := make([]string, len(prefix))
	for i, word := range prefix {
		quoted[i] = shellPath(word)
	}
	return strings.Join(quoted, " ") + " " + invocation
}

// innerCommand is the OMP command the shim runs: the launch prefix and invocation, `--resume` on
// the recorded session file when resuming, RPC mode, and the prompt word (runtime-tmux.ts:414-431).
func innerCommand(prefix []string, invocation, resumeSessionFile string, parts runtime.PromptParts) string {
	resume := ""
	if resumeSessionFile != "" {
		resume = " --resume=" + shellPath(resumeSessionFile)
	}
	return WithOmpLaunchPrefix(prefix, invocation) + resume + " --mode rpc " + systemPromptArgument(parts)
}

// shimShellCommand is the pane's shell command: PATH exported first, then the workspace, then
// `legion worker-shim` dialing the daemon's stream listener with the pane's boot token file, around
// the inner command (runtime-tmux.ts:589-601).
//
// PATH is exported here because it cannot ride a -e pair: tmux copies the -e pairs into a new
// pane's environment and then replaces PATH from the spawning client's own (spawn.c, "The session
// one is replaced from the client if there is one"), so a `-e PATH=…` never reaches a pane
// (LEGION-91, runtime-tmux.ts:603-618). An empty path exports nothing.
//
// The inner command is spliced in unquoted on purpose: the pane's shell expands its `$(cat …)`
// prompt word and word-splits the rest, so the shim receives OMP's argv, not one string.
func shimShellCommand(path, workspace, legion, streamAddress, bootTokenFile, inner string) string {
	export := ""
	if path != "" {
		export = "export PATH=" + shellPath(path) + " && "
	}
	return export + "cd " + shellPath(workspace) + " && " + shellPath(legion) +
		" worker-shim --connect " + shellPath(streamAddress) +
		" --boot-token-file " + shellPath(bootTokenFile) +
		" -- " + inner
}
