package main

import (
	"context"
	"fmt"
	"io"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/handoffimport"
)

// handoffsCommands is `legion handoffs`, the move of the workers' committed handoff files onto
// their Dispatch issues (INFRA-177), and the one list of its subcommands.
var handoffsCommands = map[string]command{
	"import": runHandoffsImport,
}

func runHandoffs(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runSubcommand(ctx, "handoffs", handoffsCommands, args, stdout, stderr)
}

// runHandoffsImport is `legion handoffs import --config <legion.yaml> [--dry-run]`
// (handoffimport.Run): from the daemon's own configuration — its project, dispatch_url and
// dispatch_token_file, the implement GitHub App, and daemon_url (or the port's loopback address) —
// every issue the running daemon holds in a phase other than done gets its branch's
// `.legion/<KEY>/<word>.json` files written to Dispatch under the daemon's session, and then one
// commit removing `.legion/` from its branch. The configuration is read by config.Load, as boot
// reads it, since minting the App's token runs its private_key_command. docs/kubernetes.md has the
// run order around a release.
func runHandoffsImport(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("handoffs import", "usage: legion handoffs import [--config <legion.yaml>] [--dry-run]", stderr)
	configPath := flags.String("config", defaultConfigPath, "path to legion.yaml")
	dryRun := flags.Bool("dry-run", false, "read everything and print what the import would do, writing nothing")
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "legion handoffs import: unexpected argument %q; name the configuration with --config <file>\n", flags.Arg(0))
		return 2
	}
	cfg, err := config.Load(*configPath, nil)
	if err != nil {
		fmt.Fprintf(stderr, "legion handoffs import: %v\n", err)
		return 1
	}
	if cfg.DispatchURL == "" {
		fmt.Fprintf(stderr, "legion handoffs import: %s names no dispatch_url, the Dispatch the handoffs go to\n", *configPath)
		return 1
	}
	token, err := config.ReadSecretPointer("dispatch_token_file", cfg.DispatchTokenFile)
	if err != nil {
		fmt.Fprintf(stderr, "legion handoffs import: %v\n", err)
		return 1
	}
	return handoffimport.Run(ctx, handoffimport.Options{
		Project:   cfg.Project,
		DaemonURL: cfg.DaemonURL,
		Tokens:    appauth.New(cfg.GitHubApps, appauth.Options{}),
		Dispatch:  dispatch.New(cfg.DispatchURL, token),
		DryRun:    *dryRun,
	}, stdout, stderr)
}
