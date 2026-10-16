// Command pulumi-resource-envoy-infra is the envoy-infra Pulumi plugin: Pulumi starts it and
// talks to it over gRPC.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/sjawhar/envoy/infra/provider"
)

var version = "0.0.0-dev" // -ldflags "-X main.version=<X.Y.Z>" in release-envoy-infra.yaml's build job

func main() {
	prov, err := provider.New()
	if err != nil {
		fmt.Fprintf(os.Stderr, "envoy-infra: %v\n", err)
		os.Exit(1)
	}
	if err := prov.Run(context.Background(), "envoy-infra", version); err != nil {
		fmt.Fprintf(os.Stderr, "envoy-infra: %v\n", err)
		os.Exit(1)
	}
}
