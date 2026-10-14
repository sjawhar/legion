// Command envoy-infra is envoy-infra's command line. Its one command, seed, writes a NATS nkey user
// seed into a secret store and checks and verifies the one stored there (package seed).
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/sjawhar/envoy/infra/seed"
)

func main() {
	stores := seed.Stores{seed.AWSSecretsManager: seed.OpenAWS}
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "seed" {
		os.Exit(seed.Command(context.Background(), args[1:], os.Stdout, os.Stderr, stores))
	}
	help := len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help")
	if !help {
		// The word given is not echoed: a seed pasted there must not reach the terminal.
		fmt.Fprintln(os.Stderr, "envoy-infra: the command is seed")
	}
	seed.Command(context.Background(), []string{"--help"}, os.Stdout, os.Stderr, stores)
	if help {
		os.Exit(0)
	}
	os.Exit(2)
}
