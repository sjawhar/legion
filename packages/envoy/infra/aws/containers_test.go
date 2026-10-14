package aws

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
)

// The goldens are the reference implementation's own container definitions, rendered from the
// placeholder inputs below with its config header replaced by goldenHeader.

// goldenHeader is the header the goldens' start script renders.
var goldenHeader = []string{"Rendered by envoy-infra from placeholder inputs.", "Public keys only."}

// goldenServerArgs and goldenConfigPath are the reference's server arguments and config path.
var goldenServerArgs = []string{"--jetstream", "--store_dir", "/data", "--port", "4222", "--http_port", "8222"}

const goldenConfigPath = "/tmp/nats-server.conf"

// goldenAuthorization is Task 2.1's placeholder authorization: public keys only.
func goldenAuthorization() core.Authorization {
	return core.Authorization{
		Users: []core.User{
			{
				Name:      "example-listener",
				NKey:      "UBERXOSHFHFLL7YEXXH7RYEJLK455TF4BOZLIHD5R6NF5FFISNHEXIZO",
				Publish:   &core.Rules{Allow: []string{"envoy.example.>"}, Deny: []string{"envoy.example.private"}},
				Subscribe: &core.Rules{Allow: []string{"_INBOX.>", "notifications.café.>"}},
				HasSeed:   true,
			},
			{
				Name:      "example-daemon",
				NKey:      "UCYF4CJJNU55NFRALQXGPOAZRM5R5JBXI6OAZB2BHWJ4QTTB5XHFENDG",
				Publish:   &core.Rules{Allow: []string{"$JS.API.>"}},
				Subscribe: &core.Rules{Allow: []string{"_INBOX.>", "legion.work workers"}},
				HasSeed:   true,
			},
			{
				Name:      "example-fallback",
				NKey:      "UDCQRANOFSHHJXENVBTXNFSRUDIUXGXPNWC5ABJFJ7GBPO27F7UIBVXD",
				Publish:   &core.Rules{Allow: []string{">"}},
				Subscribe: &core.Rules{Allow: []string{">"}},
			},
			{Name: "example-pending", HasSeed: true},
		},
		NoAuthUser: "example-fallback",
	}
}

// goldenContainers are the placeholder inputs the goldens were rendered from, launch aside.
func goldenContainers(launch []core.Field) containerConfig {
	return containerConfig{
		NatsImage:        "example.registry/nats:placeholder",
		Launch:           launch,
		TailscaleImage:   "example.registry/tailscale@sha256:" + strings.Repeat("0", 64),
		TailnetHostname:  "envoy-nats",
		TailnetTags:      []string{"tag:example-a", "tag:example-b"},
		TailnetSecretArn: "arn:aws:secretsmanager:example-region-1:000000000000:secret:example/tailnet-oauth",
		LogGroup:         "/ecs/envoy-nats",
		Region:           "example-region-1",
	}
}

func readGolden(t *testing.T, name string) string {
	t.Helper()
	text, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return string(text)
}

func TestContainerDefinitionsMatchTheReferenceGoldens(t *testing.T) {
	authorization := goldenAuthorization()
	for _, c := range []struct {
		golden        string
		authorization *core.Authorization
	}{
		{"nats-authorized.golden", &authorization},
		{"nats-unauthorized.golden", nil},
	} {
		t.Run(c.golden, func(t *testing.T) {
			launch, err := natsLaunch(c.authorization, goldenHeader, goldenServerArgs, goldenConfigPath)
			require.NoError(t, err)
			require.Equal(t, readGolden(t, c.golden), containerDefinitions(goldenContainers(launch)))
		})
	}
}

// The reference's auth key is never ephemeral; tailnetEphemeral changes that one value of the
// sidecar's script and nothing else.
func TestContainerDefinitionsEphemeralTailnetKey(t *testing.T) {
	launch, err := natsLaunch(nil, goldenHeader, goldenServerArgs, goldenConfigPath)
	require.NoError(t, err)
	config := goldenContainers(launch)
	config.TailnetEphemeral = true
	golden := readGolden(t, "nats-unauthorized.golden")
	require.Equal(t, 1, strings.Count(golden, "?ephemeral=false&"))
	require.Equal(t, strings.Replace(golden, "?ephemeral=false&", "?ephemeral=true&", 1), containerDefinitions(config))
}

func TestNatsLaunch(t *testing.T) {
	t.Run("without authorization the image's entrypoint runs nats-server with the arguments", func(t *testing.T) {
		launch, err := natsLaunch(nil, nil, []string{"-js", "-m", "8222"}, "")
		require.NoError(t, err)
		require.Equal(t, []core.Field{{Key: "command", Value: []string{"nats-server", "-js", "-m", "8222"}}}, launch)
	})
	t.Run("with authorization a shell writes the configuration and execs nats-server on it", func(t *testing.T) {
		authorization := goldenAuthorization()
		launch, err := natsLaunch(&authorization, goldenHeader, goldenServerArgs, goldenConfigPath)
		require.NoError(t, err)
		script, err := authorization.StartScript(goldenHeader, goldenServerArgs, goldenConfigPath)
		require.NoError(t, err)
		require.Equal(t, []core.Field{
			{Key: "entryPoint", Value: []string{"/bin/sh", "-c"}},
			{Key: "command", Value: []string{script}},
		}, launch)
	})
}
