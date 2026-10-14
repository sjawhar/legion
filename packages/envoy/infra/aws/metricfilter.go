package aws

import (
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/cloudwatch"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/sjawhar/envoy/infra/aws/rendering"
)

// newMetricFilter registers f on the log group logGroup names. Each matching line counts 1, and a
// period whose lines all miss counts 0.
func newMetricFilter(ctx *pulumi.Context, f rendering.MetricFilter, logGroup pulumi.StringInput, opts []pulumi.ResourceOption) error {
	_, err := cloudwatch.NewLogMetricFilter(ctx, f.LogicalName, &cloudwatch.LogMetricFilterArgs{
		LogGroupName: logGroup,
		Pattern:      pulumi.String(f.Pattern),
		MetricTransformation: cloudwatch.LogMetricFilterMetricTransformationArgs{
			Name:         pulumi.String(f.MetricName),
			Namespace:    pulumi.String(f.MetricNamespace),
			Value:        pulumi.String("1"),
			DefaultValue: pulumi.String("0"),
		},
	}, opts...)
	return err
}
