// Package nats holds the schema types every NATS component takes, whatever cloud it runs on, so a
// consumer running more than one spells a user's rules alike and a single-cloud consumer's SDK
// shows no other cloud's types. It imports no cloud package.
package nats

import (
	"github.com/pulumi/pulumi-go-provider/infer"

	"github.com/sjawhar/envoy/infra/core"
)

// RulesArgs are one direction's permissions of a NATS user, the schema type nats:Rules. They mirror
// core.Rules field for field.
type RulesArgs struct {
	Allow []string `pulumi:"allow"`
	Deny  []string `pulumi:"deny,optional"`
}

// Annotate names the type nats:Rules in the schema: generated SDKs append Args to an object type's
// name for its input type, so the Go name would reach consumers as RulesArgsArgs.
func (r *RulesArgs) Annotate(an infer.Annotator) { an.SetToken("nats", "Rules") }

// Core returns the rules as core.Rules takes them, nil for nil.
func (r *RulesArgs) Core() *core.Rules {
	if r == nil {
		return nil
	}
	return &core.Rules{Allow: r.Allow, Deny: r.Deny}
}
