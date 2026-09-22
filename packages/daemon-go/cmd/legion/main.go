package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
)

type command func(ctx context.Context, args []string, stdout, stderr io.Writer) int

var commands = map[string]command{
	"version": runVersion,
}

func run(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	if len(argv) < 2 {
		fmt.Fprintln(stderr, "usage: legion <command> [flags]")
		return 2
	}
	cmd, ok := commands[argv[1]]
	if !ok {
		fmt.Fprintf(stderr, "legion: unknown command %q\n", argv[1])
		return 2
	}
	return cmd(ctx, argv[2:], stdout, stderr)
}

func runVersion(_ context.Context, _ []string, stdout, _ io.Writer) int {
	version := "(devel)"
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		version = info.Main.Version
	}
	fmt.Fprintf(stdout, "legion %s\n", version)
	return 0
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args, os.Stdout, os.Stderr))
}
