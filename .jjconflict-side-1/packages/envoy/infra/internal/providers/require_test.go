package providers_test

import (
	"testing"

	"github.com/pulumi/pulumi-command/sdk/go/command"
	"github.com/pulumi/pulumi-docker/sdk/v5/go/docker"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/internal/providers"
	"github.com/sjawhar/envoy/infra/internal/pulumitest"
)

// run runs program on the shared mock engine and returns its error.
func run(program pulumi.RunFunc) error {
	return (&pulumitest.Mocks{}).Run(program)
}

func TestRequireFindsTheProviderForThePackage(t *testing.T) {
	err := run(func(ctx *pulumi.Context) error {
		prov, err := docker.NewProvider(ctx, "docker-host", &docker.ProviderArgs{})
		if err != nil {
			return err
		}
		return providers.Require(ctx.Context(), "envoy-infra:host:Listener", "docker", pulumi.Providers(prov))
	})
	require.NoError(t, err)
}

func TestRequireFindsTheProviderAmongProvidersOfOtherPackages(t *testing.T) {
	err := run(func(ctx *pulumi.Context) error {
		cmd, err := command.NewProvider(ctx, "command-host", &command.ProviderArgs{})
		if err != nil {
			return err
		}
		prov, err := docker.NewProvider(ctx, "docker-host", &docker.ProviderArgs{})
		if err != nil {
			return err
		}
		return providers.Require(ctx.Context(), "envoy-infra:host:Listener", "docker", pulumi.Providers(cmd, prov))
	})
	require.NoError(t, err)
}

func TestRequireFindsTheProviderPassedAsTheProviderOption(t *testing.T) {
	err := run(func(ctx *pulumi.Context) error {
		prov, err := docker.NewProvider(ctx, "docker-host", &docker.ProviderArgs{})
		if err != nil {
			return err
		}
		return providers.Require(ctx.Context(), "envoy-infra:host:Listener", "docker", pulumi.Provider(prov))
	})
	require.NoError(t, err)
}

func TestRequireRefusesAMissingProviderNamingComponentAndPackage(t *testing.T) {
	err := run(func(ctx *pulumi.Context) error {
		return providers.Require(ctx.Context(), "envoy-infra:host:Listener", "docker")
	})
	require.ErrorContains(t, err, `envoy-infra:host:Listener needs a "docker" provider in its providers option`)
}

// The command provider is named "docker", so only its type, pulumi:providers:command, refuses it.
func TestRequireRefusesAProviderOfAnotherPackage(t *testing.T) {
	err := run(func(ctx *pulumi.Context) error {
		cmd, err := command.NewProvider(ctx, "docker", &command.ProviderArgs{})
		if err != nil {
			return err
		}
		return providers.Require(ctx.Context(), "envoy-infra:host:Listener", "docker", pulumi.Providers(cmd))
	})
	require.ErrorContains(t, err, `envoy-infra:host:Listener needs a "docker" provider in its providers option`)
}
