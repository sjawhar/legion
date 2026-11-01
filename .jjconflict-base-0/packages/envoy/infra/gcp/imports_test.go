package gcp_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The hub is a GCP component: it shares the cloud-neutral nats package with the AWS components and
// compiles none of theirs, so a GCP-only consumer's plugin build and SDK hold no AWS type. go list
// lists the gcp package's whole import graph, its tests' imports aside.
func TestTheHubCompilesNoAWSPackage(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", ".").CombinedOutput()
	require.NoError(t, err, string(out))
	deps := strings.Fields(string(out))
	require.Contains(t, deps, "github.com/sjawhar/envoy/infra/nats", "the hub takes the shared rules type")
	for _, dep := range deps {
		require.False(t, dep == "github.com/sjawhar/envoy/infra/aws" || strings.HasPrefix(dep, "github.com/sjawhar/envoy/infra/aws/"),
			"the gcp package imports %s", dep)
	}
}
