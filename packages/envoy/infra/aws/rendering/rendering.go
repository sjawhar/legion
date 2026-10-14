// Package rendering holds what the AWS components render, with no Pulumi in it: the tasks'
// container definitions, the NATS start script, the listener's environment and its secret-read
// policy, and the text of the log metric filters and alarms. The components in package aws
// register them, and `envoy-infra render` prints the parts a deployment's own checks read, from
// the same functions, so the command links neither Pulumi nor the AWS provider's SDK.
//
// The AWS provider compares every string a component registers byte for byte, so each text here
// is written exactly as the reference implementation wrote it. Its tests are package aws's, which
// build a component and its renderings from one example.
package rendering

// MetricFilter is one log metric filter a component registers on its log group: the pattern of
// the log lines it counts and the metric it counts them into. `envoy-infra render metric-filters
// --json` prints a component's filters under these fields' JSON names.
type MetricFilter struct {
	LogicalName     string `json:"logicalName"`
	Pattern         string `json:"pattern"`
	MetricName      string `json:"metricName"`
	MetricNamespace string `json:"metricNamespace"`
}
