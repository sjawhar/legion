package core_test

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/testnats"
)

var updateGrantFixtures = flag.Bool("update", false, "rewrite the grant fixtures in packages/envoy/internal/testnats/testdata")

// grantFixtureDir is where Envoy's tests read the grant fixtures, from this package's directory.
var grantFixtureDir = filepath.Join("..", "..", "internal", "testnats", "testdata")

// Envoy's tests run its processes under the grants this module renders, which they read from
// fixtures because the envoy module cannot import this one (testnats.ListenerGrant,
// testnats.HostGrant). Each fixture must hold its grant rendered for testnats.MachinePlaceholder,
// byte for byte, so a grant change that leaves a fixture behind fails here; -update rewrites them.
func TestTheTestGrantFixturesAreTheRenderedGrants(t *testing.T) {
	for name, permissions := range map[string]core.Permissions{
		testnats.ListenerGrantFixture: core.ListenerPermissions(testnats.MachinePlaceholder),
		testnats.HostGrantFixture:     core.HostPermissions(testnats.MachinePlaceholder),
	} {
		t.Run(name, func(t *testing.T) {
			want, err := testnats.EncodeGrant(testnats.Grant{
				Publish:   testnats.GrantRules{Allow: permissions.Publish.Allow, Deny: permissions.Publish.Deny},
				Subscribe: testnats.GrantRules{Allow: permissions.Subscribe.Allow, Deny: permissions.Subscribe.Deny},
			})
			require.NoError(t, err)
			path := filepath.Join(grantFixtureDir, name)
			if *updateGrantFixtures {
				require.NoError(t, os.WriteFile(path, want, 0o644))
			}
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, string(want), string(got),
				"%s is not the grant rendered for %s; rewrite it with: go test ./core -run TestTheTestGrantFixturesAreTheRenderedGrants -update",
				path, testnats.MachinePlaceholder)
		})
	}
}
