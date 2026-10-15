//go:build natslive

package seed_test

import (
	"context"
	"testing"

	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/seed"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The handoff a consumer relies on: the server's authorization takes the public key Generate
// returned, and a client takes the seed the store holds. That client connects; one with no
// credential, or with another user's well-formed seed, is refused. On the 2.10 image and the hub's.
func TestAGeneratedSeedReachesNATS(t *testing.T) {
	for _, image := range testnats.Images {
		t.Run(image.Name, func(t *testing.T) {
			store := newStore(t)
			public, version, err := seed.Generate(context.Background(), store, secretName, quiet)
			require.NoError(t, err)
			require.Equal(t, "v1", version)
			require.Len(t, store.puts, 1)
			stored := string(store.puts[0])

			config, err := core.Authorization{Users: []core.User{{
				Name:      "example-host-a",
				NKey:      public,
				Publish:   &core.Rules{Allow: []string{">"}},
				Subscribe: &core.Rules{Allow: []string{">"}},
				HasSeed:   true,
			}}}.Render([]string{"Rendered by envoy-infra from placeholder inputs.", "Public keys only."})
			require.NoError(t, err)
			require.NotContains(t, config, stored, "the server's configuration holds the public key only")
			_, uri := testnats.StartImageConfig(t, image.Ref, config, []string{"-js"}, false)

			t.Run("a client with the stored seed connects", func(t *testing.T) {
				conn, err := testnats.ConnectSeed(t, uri, stored)
				require.NoError(t, err)
				defer conn.Close()
				require.NoError(t, conn.Publish("envoy.example.probe", []byte("hello")))
				require.NoError(t, conn.Flush())
			})
			t.Run("a client with no credential is refused", func(t *testing.T) {
				_, err := testnats.ConnectSeed(t, uri, "")
				require.ErrorIs(t, err, natsgo.ErrAuthorization)
			})
			t.Run("a client with another user's seed is refused", func(t *testing.T) {
				other, _ := testnats.User(t)
				_, err := testnats.ConnectSeed(t, uri, other)
				require.ErrorIs(t, err, natsgo.ErrAuthorization)
			})
		})
	}
}
