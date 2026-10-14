package aws_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	envoyaws "github.com/sjawhar/envoy/infra/aws"
	"github.com/sjawhar/envoy/infra/core"
)

// The goldens are the reference implementation's own container definitions, rendered from
// exampleArgs' values with its config header replaced by exampleArgs' configHeader.

// containers returns the container configuration args give: the values NewNatsService passes.
func containers(t *testing.T, args *envoyaws.NatsServiceArgs) envoyaws.ContainerConfig {
	t.Helper()
	var authorization *core.Authorization
	if args.Authorization != nil {
		authorization = args.Authorization.Core()
	}
	launch, err := envoyaws.NatsLaunch(authorization, args.ConfigHeader, args.ServerArgs, args.ConfigPath)
	require.NoError(t, err)
	return envoyaws.ContainerConfig{
		NatsImage:        args.NatsImage,
		Launch:           launch,
		TailscaleImage:   args.TailscaleImage,
		TailnetHostname:  args.TailnetHostname,
		TailnetTags:      args.TailnetTags,
		TailnetEphemeral: args.TailnetEphemeral,
		TailnetSecretArn: literal(t, args.TailnetClientSecretArn),
		LogGroup:         args.LogGroupName,
		Region:           args.Region,
	}
}

func TestContainerDefinitionsMatchTheReferenceGoldens(t *testing.T) {
	for _, c := range []struct {
		golden        string
		authorization *envoyaws.AuthorizationArgs
	}{
		{"nats-authorized.golden", exampleAuthorization()},
		{"nats-unauthorized.golden", nil},
	} {
		t.Run(c.golden, func(t *testing.T) {
			args := exampleArgs()
			args.Authorization = c.authorization
			require.Equal(t, readGolden(t, c.golden), envoyaws.ContainerDefinitions(containers(t, args)))
		})
	}
}

// The reference's auth key is never ephemeral; tailnetEphemeral changes that one value of the
// sidecar's script and nothing else.
func TestContainerDefinitionsEphemeralTailnetKey(t *testing.T) {
	args := exampleArgs()
	args.Authorization = nil
	args.TailnetEphemeral = true
	golden := readGolden(t, "nats-unauthorized.golden")
	require.Equal(t, 1, strings.Count(golden, "?ephemeral=false&"))
	require.Equal(t, strings.Replace(golden, "?ephemeral=false&", "?ephemeral=true&", 1),
		envoyaws.ContainerDefinitions(containers(t, args)))
}

func TestNatsLaunch(t *testing.T) {
	t.Run("without authorization the image's entrypoint runs nats-server with the arguments", func(t *testing.T) {
		launch, err := envoyaws.NatsLaunch(nil, nil, []string{"-js", "-m", "8222"}, "")
		require.NoError(t, err)
		require.Equal(t, []core.Field{{Key: "command", Value: []string{"nats-server", "-js", "-m", "8222"}}}, launch)
	})
	t.Run("with authorization a shell writes the configuration and execs nats-server on it", func(t *testing.T) {
		args := exampleArgs()
		authorization := args.Authorization.Core()
		launch, err := envoyaws.NatsLaunch(authorization, args.ConfigHeader, args.ServerArgs, args.ConfigPath)
		require.NoError(t, err)
		script, err := authorization.StartScript(args.ConfigHeader, args.ServerArgs, args.ConfigPath)
		require.NoError(t, err)
		require.Equal(t, []core.Field{
			{Key: "entryPoint", Value: []string{"/bin/sh", "-c"}},
			{Key: "command", Value: []string{script}},
		}, launch)
	})
}
