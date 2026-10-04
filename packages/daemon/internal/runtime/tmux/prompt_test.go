package tmux

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// The OMP command inside the shim, as runtime-tmux.ts:430 builds it: prefix and invocation,
// `--resume=<file>` when resuming, `--mode rpc`, the prompt word (processes.test.ts:1126 fresh,
// :3950 resumed).
func TestInnerCommand(t *testing.T) {
	parts := runtime.PromptParts{RolePromptPaths: []string{"/roles/architect-root.md"}}
	prompt := `--append-system-prompt "$(cat /roles/architect-root.md)"`
	if got, want := innerCommand(nil, "/opt/oh-my-pi/18.0.3/omp", "", parts),
		"/opt/oh-my-pi/18.0.3/omp --mode rpc "+prompt; got != want {
		t.Errorf("fresh: got %q, want %q", got, want)
	}
	if got, want := innerCommand([]string{"env", "K=v", "--"}, "/opt/oh-my-pi/18.0.3/omp", "/state/trees/x/.omp/s 1.jsonl", parts),
		"env 'K=v' -- /opt/oh-my-pi/18.0.3/omp --resume='/state/trees/x/.omp/s 1.jsonl' --mode rpc "+prompt; got != want {
		t.Errorf("resumed: got %q, want %q", got, want)
	}
}

// The pane's shell command: PATH exported before anything runs (tmux replaces a pane's PATH from
// the spawning client after copying the -e pairs — LEGION-91, runtime-tmux.ts:609-625), then the
// workspace, then the shim around the inner command (runtime-tmux.ts:599; the shim now dials the
// daemon's stream listener with the pane's boot token rather than serving a socket). With provider
// keys, the shim is pointed at the daemon-held directory whose files it exports into OMP's
// environment alone; without, the flag is absent.
func TestShimShellCommand(t *testing.T) {
	got := shimShellCommand(
		"legion-omp",
		"/state dir/bin:/usr/bin",
		"/state/workspaces/LEGION-42",
		"/opt/legion/legion",
		"unix:///state/worker-stream.sock",
		"/state/secrets/legion-omp-LEGION-42-architect",
		"/state dir/secrets/provider-env",
		"omp --mode rpc",
	)
	want := "tmux set-option -w -t \"$TMUX_PANE\" @legion_owner legion-omp && export PATH='/state dir/bin:/usr/bin' && cd /state/workspaces/LEGION-42 && /opt/legion/legion worker-shim" +
		" --connect unix:///state/worker-stream.sock --boot-token-file /state/secrets/legion-omp-LEGION-42-architect" +
		" --provider-env-dir '/state dir/secrets/provider-env' -- omp --mode rpc"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if got := shimShellCommand("legion-omp", "", "/w", "/l", "unix:///s", "/t", "", "omp"); got != "tmux set-option -w -t \"$TMUX_PANE\" @legion_owner legion-omp && cd /w && /l worker-shim --connect unix:///s --boot-token-file /t -- omp" {
		t.Errorf("no PATH, no provider keys: got %q", got)
	}
}
