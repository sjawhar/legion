// Package providers holds the check every component makes before it registers anything: that the
// consumer passed it a provider for each package it creates resources with, so no resource falls
// back to a default provider of the plugin's own SDK version.
package providers

import (
	"context"
	"fmt"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/internals"
)

// Require returns nil when opts carry a provider of package pkg, and otherwise an error naming
// component and pkg. It reads the providers option, which the provider option adds its provider
// to as well. A provider's package is the last part of its type, pulumi:providers:<pkg>, read from
// its URN: a component served by the plugin receives each provider as a reference whose URN is all
// it has.
func Require(ctx context.Context, component, pkg string, opts ...pulumi.ResourceOption) error {
	ro, err := pulumi.NewResourceOptions(opts...)
	if err != nil {
		return fmt.Errorf("%s: read its resource options: %w", component, err)
	}
	for _, prov := range ro.Providers {
		out, err := internals.UnsafeAwaitOutput(ctx, prov.URN())
		if err != nil {
			return fmt.Errorf("%s: read the URN of a provider it was given: %w", component, err)
		}
		urn, ok := out.Value.(pulumi.URN)
		if !ok {
			return fmt.Errorf("%s: a provider it was given has no known URN", component)
		}
		if resource.URN(urn).Type().Name() == tokens.TypeName(pkg) {
			return nil
		}
	}
	return fmt.Errorf("%s needs a %q provider in its providers option", component, pkg)
}
