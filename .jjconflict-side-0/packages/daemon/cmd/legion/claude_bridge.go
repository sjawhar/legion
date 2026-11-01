package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/sjawhar/legion/daemon/internal/claudebridge"
)

const claudeBridgeUsage = "usage: legion claude-bridge -- <claude argv…>"

// runClaudeBridge is `legion claude-bridge`, which `legion worker-shim` runs in Oh My Pi's place for
// a phase worker whose role runs on Claude Code (`worker_backends`): Oh My Pi's RPC frames on its
// stdin and stdout, Claude Code's stream-json to the `claude -p` after `--` (internal/claudebridge).
// Its own lines and Claude Code's stderr go to stderr, which the pane shows; its exit status is
// Claude Code's. SIGHUP ends it as SIGTERM does: every process of a tmux pane gets it when the pane
// or its server goes, and the bridge's shutdown is what ends Claude Code and removes its inbox.
func runClaudeBridge(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGHUP)
	defer stop()
	flags := newFlags("claude-bridge", claudeBridgeUsage, stderr)
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}
	cfg, err := claudebridge.Configure(flags.Args(), os.Environ())
	if err != nil {
		fmt.Fprintf(stderr, "legion claude-bridge: %v\n%s\n", err, claudeBridgeUsage)
		return 2
	}
	frames, release, err := ownDescriptor(stdout)
	if err != nil {
		fmt.Fprintf(stderr, "legion claude-bridge: %v\n", err)
		return 1
	}
	defer release()
	cfg.Stdin, cfg.Stdout, cfg.Log = os.Stdin, frames, stderr
	code, err := claudebridge.Run(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "legion claude-bridge: %v\n", err)
		return 1
	}
	return code
}

// ownDescriptor is where the bridge writes its frames to the shim: stdout, and when stdout is a file,
// a close-on-exec duplicate of it on a descriptor of its own, which release closes. Go ends a process
// with SIGPIPE when a write to descriptor 1 or 2 finds no reader, and a pane's shim dies with the
// pane while the bridge still answers Claude Code's last lines; on any other descriptor that write
// fails with EPIPE, which the bridge logs and outlives, so its shutdown still ends Claude Code and
// removes its inbox. release never closes stdout itself.
func ownDescriptor(stdout io.Writer) (frames io.Writer, release func(), err error) {
	file, ok := stdout.(*os.File)
	if !ok {
		return stdout, func() {}, nil
	}
	fd, err := unix.FcntlInt(file.Fd(), unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		return nil, nil, fmt.Errorf("duplicate %s: %w", file.Name(), err)
	}
	duplicate := os.NewFile(uintptr(fd), file.Name())
	return duplicate, func() { _ = duplicate.Close() }, nil
}
