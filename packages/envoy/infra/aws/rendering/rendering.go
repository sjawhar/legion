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

import "github.com/sjawhar/envoy/infra/core"

// Owner is what an OwnedInput belongs to: a webhook, or a NatsService's authorization.
type Owner struct {
	Name    string // as a refusal names it: `webhook "github"`, `authorization`
	Present bool   // the webhook is listed; the authorization is given
	Absent  string // what a refusal says of the owner when it is not present
}

// OwnedInput is one input that belongs to an Owner: the component reads it only while its owner is
// present.
type OwnedInput struct {
	Owner      Owner
	Field      string
	Given, Set bool // given at all; given with a value the component can use
}

// CheckOwnedInputs refuses, in order, each input whose owner is present and that is not set, and
// each input whose owner is not present and that is given, which would otherwise be dropped
// silently. Each refusal is a core.InputError.
func CheckOwnedInputs(inputs []OwnedInput) error {
	for _, in := range inputs {
		if in.Owner.Present && !in.Set {
			return core.InputErrorf(in.Field, "%s is required with %s", in.Field, in.Owner.Name)
		}
		if !in.Owner.Present && in.Given {
			return core.InputErrorf(in.Field, "%s is given for %s, %s", in.Field, in.Owner.Name, in.Owner.Absent)
		}
	}
	return nil
}
