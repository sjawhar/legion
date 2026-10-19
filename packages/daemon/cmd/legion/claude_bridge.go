package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/sjawhar/legion/daemon/internal/claudebridge"
)

const claudeBridgeUsage = "usage: legion claude-bridge -- <claude argv…>"

// runClaudeBridge is `legion claude-bridge`, which `legion worker-shim` runs in Oh My Pi's place for
// a phase worker whose role runs on Claude Code (`worker_backends`): Oh My Pi's RPC frames on its
// stdin and stdout, Claude Code's stream-json to the `claude -p` after `--` (internal/claudebridge).
// Its own lines and Claude Code's stderr go to stderr, which the pane shows; its exit status is
// Claude Code's.
func runClaudeBridge(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("claude-bridge", claudeBridgeUsage, stderr)
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}
	cfg, err := claudebridge.Configure(flags.Args(), os.Environ())
	if err != nil {
		fmt.Fprintf(stderr, "legion claude-bridge: %v\n%s\n", err, claudeBridgeUsage)
		return 2
	}
	cfg.Stdin, cfg.Stdout, cfg.Log = os.Stdin, stdout, stderr
	code, err := claudebridge.Run(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "legion claude-bridge: %v\n", err)
		return 1
	}
	return code
}
