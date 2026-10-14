// Command envoy-infra is envoy-infra's command line. seed writes a NATS nkey user seed into a
// secret store and checks and verifies the one stored there (package seed); render prints what the
// AWS components register, for a program that needs it outside Pulumi (package render); and
// `envoy-infra --version` prints the release it was built as.
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/sjawhar/envoy/infra/render"
	"github.com/sjawhar/envoy/infra/seed"
)

var version = "0.0.0-dev" // -ldflags "-X main.version=<X.Y.Z>" in the release job

func main() {
	stores := seed.Stores{seed.AWSSecretsManager: seed.OpenAWS}
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr, stores))
}

// run runs the command line args, the words after the program's name, and returns its exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, stores seed.Stores) int {
	if len(args) > 0 && args[0] == "seed" {
		return seed.Command(ctx, args[1:], stdout, stderr, stores)
	}
	if len(args) > 0 && args[0] == "render" {
		return render.Command(args[1:], stdout, stderr)
	}
	if len(args) == 1 && args[0] == "--version" {
		fmt.Fprintln(stdout, version)
		return 0
	}
	help := len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help")
	if !help {
		// The words given are not echoed: a seed pasted there must not reach the terminal.
		fmt.Fprintln(stderr, "envoy-infra: the command is seed or render, or --version alone")
	}
	fmt.Fprintln(stderr, "usage: envoy-infra --version")
	seed.Command(ctx, []string{"--help"}, stdout, stderr, stores)
	render.Command([]string{"--help"}, stdout, stderr)
	if help {
		return 0
	}
	return 2
}
