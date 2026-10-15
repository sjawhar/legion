// Package rendering holds what the AWS components render, with no Pulumi in it: the tasks'
// container definitions, the NATS start script, the listener's environment and its secret-read
// policy, and the text of the log metric filters and alarms. The components in package aws
// register them, and `envoy-infra render` prints the parts a deployment's own checks read, from
// the same functions, so the command links neither Pulumi nor the AWS provider's SDK.
//
// The AWS provider compares every string a component registers byte for byte, so each text here
// is written exactly as the reference implementation wrote it. Package aws's tests pin the texts,
// building each component from one example; this package's own tests hold ListenerEnv.Check's
// refusals.
package rendering
