package rendering

import "github.com/sjawhar/envoy/infra/core"

// The parts of an ECS container definition both components' containers write, each as an object
// in the reference's dict order.

func portMapping(port int) core.Object {
	return core.Object{{Key: "containerPort", Value: port}, {Key: "protocol", Value: "tcp"}}
}

func mountPoint(volume, path string) core.Object {
	return core.Object{
		{Key: "sourceVolume", Value: volume},
		{Key: "containerPath", Value: path},
		{Key: "readOnly", Value: false},
	}
}

func healthCheck(command string, startPeriod int) core.Object {
	return core.Object{
		{Key: "command", Value: []string{"CMD-SHELL", command}},
		{Key: "interval", Value: 30},
		{Key: "timeout", Value: 5},
		{Key: "retries", Value: 3},
		{Key: "startPeriod", Value: startPeriod},
	}
}

func logConfiguration(group, region, streamPrefix string) core.Object {
	return core.Object{
		{Key: "logDriver", Value: "awslogs"},
		{Key: "options", Value: core.Object{
			{Key: "awslogs-group", Value: group},
			{Key: "awslogs-region", Value: region},
			{Key: "awslogs-stream-prefix", Value: streamPrefix},
		}},
	}
}

func environment(name, value string) core.Object {
	return core.Object{{Key: "name", Value: name}, {Key: "value", Value: value}}
}

func secret(name, arn string) core.Object {
	return core.Object{{Key: "name", Value: name}, {Key: "valueFrom", Value: arn}}
}
