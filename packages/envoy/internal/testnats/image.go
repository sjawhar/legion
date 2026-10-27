//go:build natslive

// The container-backed half of the rig, built only under the natslive tag. Envoy's tests run a
// nats-server in the test process (nats.go), which needs no container runtime; what is here runs
// a published NATS image instead, which the natslive lane uses to check the rig's helpers against
// the server an operator deploys. Keeping it behind the tag keeps testcontainers out of every
// other build of this package, and the guard in nats_test.go keeps it out of the rest of the
// module.

package testnats

import (
	"context"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
)

// imageConfigPath is where StartImageConfig puts a server's configuration: the path the alpine
// images' default command reads its configuration from.
const imageConfigPath = "/etc/nats/nats-server.conf"

// StartImageConfig runs image as a NATS test container whose whole configuration is config, at
// /etc/nats/nats-server.conf, with the arguments -c /etc/nats/nats-server.conf and then serverArgs
// (so -js for JetStream), removed when the test ends (even when its start fails). It returns the
// container with its client URL once the server answers a client with no credential (awaitAuth):
// with anonymous false, by refusing it; with anonymous true, by admitting it, as a configuration
// naming a no_auth_user makes it. Both official image variants take arguments that start with a
// flag as nats-server's own.
func StartImageConfig(t testing.TB, image, config string, serverArgs []string, anonymous bool) (*tcnats.NATSContainer, string) {
	t.Helper()
	ctx := context.Background()
	ctr, err := tcnats.Run(ctx, image,
		testcontainers.WithCmd(append([]string{"-c", imageConfigPath}, serverArgs...)...),
		testcontainers.WithFiles(testcontainers.ContainerFile{
			Reader:            strings.NewReader(config),
			ContainerFilePath: imageConfigPath,
			FileMode:          0o644,
		}),
		// One CPU and 512 MiB, as every container the repository's tests and proofs start is capped
		// on a shared host: a test server holds a few small streams.
		testcontainers.WithHostConfigModifier(func(hostConfig *container.HostConfig) {
			hostConfig.NanoCPUs = 1_000_000_000
			hostConfig.Memory = 512 << 20
		}),
	)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start NATS on %s: %v", image, err)
	}
	uri, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("NATS connection string: %v", err)
	}
	awaitAuth(t, uri, anonymous)
	return ctr, uri
}
