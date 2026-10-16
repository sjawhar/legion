package aws

import (
	"errors"
	"maps"

	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/cloudwatch"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/ecs"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// The parts both AWS components register identically: each child's options, its tags, the log
// group and the Fargate task definition's base. What differs between the services (their
// deployment policy, Cloud Map health check and volumes) stays in each component.

// childOptions are the options of each of comp's children: comp as the parent and, with adopt,
// an alias that takes over the root-level resource of the child's own name and type.
func childOptions(comp pulumi.Resource, adopt bool) []pulumi.ResourceOption {
	child := []pulumi.ResourceOption{pulumi.Parent(comp)}
	if adopt {
		child = append(child, pulumi.Aliases([]pulumi.Alias{{NoParent: pulumi.Bool(true)}}))
	}
	return child
}

// refuseServiceTag refuses baseTags holding service, which serviceTags sets on each child.
func refuseServiceTag(baseTags map[string]string) error {
	if _, ok := baseTags["service"]; ok {
		return errors.New(`baseTags holds "service", which the component sets on each child`)
	}
	return nil
}

// serviceTags are a child's tags: baseTags, which refuseServiceTag has passed, and service.
func serviceTags(baseTags map[string]string, service string) pulumi.StringMap {
	tags := map[string]string{"service": service}
	maps.Copy(tags, baseTags)
	return pulumi.ToStringMap(tags)
}

// newLogGroup registers <name>-log-group, the log group groupName kept retentionDays days.
func newLogGroup(ctx *pulumi.Context, name, groupName string, retentionDays int, tags pulumi.StringMap,
	child []pulumi.ResourceOption,
) (*cloudwatch.LogGroup, error) {
	return cloudwatch.NewLogGroup(ctx, name+"-log-group", &cloudwatch.LogGroupArgs{
		Name:            pulumi.String(groupName),
		RetentionInDays: pulumi.Int(retentionDays),
		Tags:            tags,
	}, child...)
}

// fargateTask is what a component's Fargate task definition is made of.
type fargateTask struct {
	family, cpu, memory           string
	executionRoleArn, taskRoleArn pulumi.StringInput
	containerDefinitions          pulumi.StringOutput
	tags                          pulumi.StringMap
}

// args returns the task definition's arguments: one awsvpc task on Fargate. A component adds what
// only it has, such as volumes.
func (t fargateTask) args() *ecs.TaskDefinitionArgs {
	return &ecs.TaskDefinitionArgs{
		Family:                  pulumi.String(t.family),
		Cpu:                     pulumi.String(t.cpu),
		Memory:                  pulumi.String(t.memory),
		NetworkMode:             pulumi.String("awsvpc"),
		RequiresCompatibilities: pulumi.StringArray{pulumi.String("FARGATE")},
		ExecutionRoleArn:        t.executionRoleArn,
		TaskRoleArn:             t.taskRoleArn,
		ContainerDefinitions:    t.containerDefinitions,
		Tags:                    t.tags,
	}
}
