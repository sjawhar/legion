package tmux

import (
	"github.com/sjawhar/legion/daemon/internal/omplaunch"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
)

// innerCommand is the OMP command the shim runs: the launch prefix and invocation, `--resume` on
// the recorded session file when resuming, RPC mode, and the prompt word (runtime-tmux.ts:414-431).
func innerCommand(prefix []string, invocation, resumeSessionFile string, parts runtime.PromptParts) string {
	resume := ""
	if resumeSessionFile != "" {
		resume = " --resume=" + shellprefix.Word(resumeSessionFile)
	}
	return omplaunch.WithPrefix(prefix, invocation) + resume + " --mode rpc " + omplaunch.SystemPromptArgument(parts)
}

// shimShellCommand is the pane's shell command: PATH exported first, then the workspace, then
// `legion worker-shim` dialing the daemon's stream listener with the pane's boot token file, around
// the inner command (runtime-tmux.ts:589-601). With provider keys, the shim is also pointed at the
// daemon-held directory whose files it exports into OMP's environment alone
// (`--provider-env-dir`); "" passes no such flag.
//
// PATH is exported here because it cannot ride a -e pair: tmux copies the -e pairs into a new
// pane's environment and then replaces PATH from the spawning client's own (spawn.c, "The session
// one is replaced from the client if there is one"), so a `-e PATH=…` never reaches a pane
// (LEGION-91, runtime-tmux.ts:603-618). An empty path exports nothing.
//
// The inner command is spliced in unquoted on purpose: the pane's shell expands its `$(cat …)`
// prompt word and word-splits the rest, so the shim receives OMP's argv, not one string. The pane
// marks its own window before it starts the shim: a crash after new-window created the pane but
// before the daemon received its report leaves an ownership marker the next daemon can reap.
func shimShellCommand(socket, path, workspace, legion, streamAddress, bootTokenFile, providerEnvDir, inner string) string {
	marker := "tmux set-option -w -t \"$TMUX_PANE\" @legion_owner " + shellprefix.Word(socket) + " && "
	export := ""
	if path != "" {
		export = "export PATH=" + shellprefix.Word(path) + " && "
	}
	providerEnv := ""
	if providerEnvDir != "" {
		providerEnv = " --provider-env-dir " + shellprefix.Word(providerEnvDir)
	}
	return marker + export + "cd " + shellprefix.Word(workspace) + " && " + shellprefix.Word(legion) +
		" worker-shim --connect " + shellprefix.Word(streamAddress) +
		" --boot-token-file " + shellprefix.Word(bootTokenFile) + providerEnv +
		" -- " + inner
}
