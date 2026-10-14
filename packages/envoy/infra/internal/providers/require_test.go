package providers_test

import (
	"testing"

	"github.com/pulumi/pulumi-command/sdk/go/command"
	"github.com/pulumi/pulumi-docker/sdk/v5/go/docker"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/internal/providers"
)

type mocks struct{}

func (mocks) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	return args.Name + "-id", args.Inputs, nil
}

func (mocks) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	return args.Args, nil
}

func TestRequireFindsTheProviderForThePackage(t *testing.T) {
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		prov, err := docker.NewProvider(ctx, "docker-host", &docker.ProviderArgs{})
		if err != nil {
			return err
		}
		return providers.Require(ctx.Context(), "envoy-infra:host:Listener", "docker", pulumi.Providers(prov))
	}, pulumi.WithMocks("p", "s", mocks{}))
	require.NoError(t, err)
}

func TestRequireFindsTheProviderAmongProvidersOfOtherPackages(t *testing.T) {
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		cmd, err := command.NewProvider(ctx, "command-host", &command.ProviderArgs{})
		if err != nil {
			return err
		}
		prov, err := docker.NewProvider(ctx, "docker-host", &docker.ProviderArgs{})
		if err != nil {
			return err
		}
		return providers.Require(ctx.Context(), "envoy-infra:host:Listener", "docker", pulumi.Providers(cmd, prov))
	}, pulumi.WithMocks("p", "s", mocks{}))
	require.NoError(t, err)
}

func TestRequireFindsTheProviderPassedAsTheProviderOption(t *testing.T) {
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		prov, err := docker.NewProvider(ctx, "docker-host", &docker.ProviderArgs{})
		if err != nil {
			return err
		}
		return providers.Require(ctx.Context(), "envoy-infra:host:Listener", "docker", pulumi.Provider(prov))
	}, pulumi.WithMocks("p", "s", mocks{}))
	require.NoError(t, err)
}

func TestRequireRefusesAMissingProviderNamingComponentAndPackage(t *testing.T) {
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		return providers.Require(ctx.Context(), "envoy-infra:host:Listener", "docker")
	}, pulumi.WithMocks("p", "s", mocks{}))
	require.ErrorContains(t, err, `envoy-infra:host:Listener needs a "docker" provider in its providers option`)
}

// The command provider is named "docker", so only its type, pulumi:providers:command, refuses it.
func TestRequireRefusesAProviderOfAnotherPackage(t *testing.T) {
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		cmd, err := command.NewProvider(ctx, "docker", &command.ProviderArgs{})
		if err != nil {
			return err
		}
		return providers.Require(ctx.Context(), "envoy-infra:host:Listener", "docker", pulumi.Providers(cmd))
	}, pulumi.WithMocks("p", "s", mocks{}))
	require.ErrorContains(t, err, `envoy-infra:host:Listener needs a "docker" provider in its providers option`)
}
