package pulumitest

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The live programs pin DockerPluginVersion, so a tarball of another version would leave the CLI
// downloading the pinned one unchecked. newHome refuses it by its name before it runs anything.
func TestNewHomeRefusesATarballOfAnotherVersion(t *testing.T) {
	t.Setenv(DockerPluginEnv, "/tmp/pulumi-resource-docker-v4.10.0-linux-amd64.tar.gz")
	dir, err := newHome()
	require.Empty(t, dir, "no PULUMI_HOME made")
	require.ErrorContains(t, err, DockerPluginEnv+"=/tmp/pulumi-resource-docker-v4.10.0-linux-amd64.tar.gz")
	require.ErrorContains(t, err, "v"+DockerPluginVersion)
}
