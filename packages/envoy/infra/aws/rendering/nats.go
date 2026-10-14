package rendering

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/infra/core"
)

// The ports nats-server serves in the task: clients, cluster routes and HTTP monitoring.
const (
	NatsClientPort  = 4222
	natsClusterPort = 6222
	natsMonitorPort = 8222
)

// tailnetLocalAddrPort is where the sidecar's containerboot serves its health check.
const tailnetLocalAddrPort = "127.0.0.1:41234"

// NatsContainers holds what a NatsService task's two containers take from the component's inputs
// and children.
type NatsContainers struct {
	NatsImage        string
	Launch           []core.Field // NatsLaunch's fields
	TailscaleImage   string
	TailnetHostname  string
	TailnetTags      []string
	TailnetEphemeral bool
	TailnetSecretArn string // the sidecar's OAuth client secret, injected as TS_OAUTH_SECRET
	LogGroup         string
	Region           string
}

// NatsContainerDefinitions returns a NatsService task's container definitions, the NATS container
// and the Tailscale sidecar, written as json.dumps writes the reference's list: its dict order,
// ensure_ascii on.
func NatsContainerDefinitions(c NatsContainers) string {
	nats := core.Object{
		{Key: "name", Value: "nats"},
		{Key: "image", Value: c.NatsImage},
		{Key: "essential", Value: true},
	}
	nats = append(nats, c.Launch...)
	nats = append(nats,
		core.Field{Key: "portMappings", Value: []core.Object{
			portMapping(NatsClientPort), portMapping(natsClusterPort), portMapping(natsMonitorPort),
		}},
		core.Field{Key: "mountPoints", Value: []core.Object{mountPoint("nats-data", "/data")}},
		core.Field{Key: "healthCheck", Value: healthCheck(
			fmt.Sprintf("wget -qO- http://localhost:%d/healthz || exit 1", natsMonitorPort), 30)},
		core.Field{Key: "logConfiguration", Value: logConfiguration(c.LogGroup, c.Region, "nats")},
	)

	// The sidecar serves the task's NATS client port on the tailnet. Its auth key is the OAuth
	// client secret with the key's attributes appended, which containerboot reads from TS_AUTHKEY.
	script := strings.Join([]string{
		"set -eu;",
		"mkdir -p /tmp/ts;",
		fmt.Sprintf(`echo '{"TCP":{"%d":{"TCPForward":"127.0.0.1:%d"}}}' > /tmp/ts/serve.json;`, NatsClientPort, NatsClientPort),
		`export TS_AUTHKEY="${TS_OAUTH_SECRET}?ephemeral=` + strconv.FormatBool(c.TailnetEphemeral) + `&preauthorized=true";`,
		"unset TS_OAUTH_SECRET;",
		"exec /usr/local/bin/containerboot",
	}, " ")
	sidecar := core.Object{
		{Key: "name", Value: "tailscale-sidecar"},
		{Key: "image", Value: c.TailscaleImage},
		{Key: "essential", Value: true},
		{Key: "entryPoint", Value: []string{"/bin/sh", "-c"}},
		{Key: "command", Value: []string{script}},
		{Key: "environment", Value: []core.Object{
			environment("TS_HOSTNAME", c.TailnetHostname),
			environment("TS_USERSPACE", "true"),
			environment("TS_STATE_DIR", "/var/lib/tailscale"),
			environment("TS_SERVE_CONFIG", "/tmp/ts/serve.json"),
			environment("TS_EXTRA_ARGS", "--advertise-tags="+strings.Join(c.TailnetTags, ",")),
			environment("TS_AUTH_ONCE", "true"),
			environment("TS_ENABLE_HEALTH_CHECK", "true"),
			environment("TS_LOCAL_ADDR_PORT", tailnetLocalAddrPort),
		}},
		{Key: "secrets", Value: []core.Object{
			{{Key: "name", Value: "TS_OAUTH_SECRET"}, {Key: "valueFrom", Value: c.TailnetSecretArn}},
		}},
		{Key: "mountPoints", Value: []core.Object{mountPoint("tsnet-state", "/var/lib/tailscale")}},
		{Key: "healthCheck", Value: healthCheck("wget -qO- http://"+tailnetLocalAddrPort+"/healthz || exit 1", 60)},
		{Key: "logConfiguration", Value: logConfiguration(c.LogGroup, c.Region, "tailscale-sidecar")},
	}
	return core.PyJSON([]core.Object{nats, sidecar}, core.PyJSONOptions{EnsureASCII: true})
}

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

// NatsLaunch returns the NATS container's launch fields. Without authorization the image's own
// entrypoint runs nats-server with serverArgs. With it, the task has no file of its own to mount,
// so a shell runs NatsStartScript.
func NatsLaunch(authorization *core.Authorization, header, serverArgs []string, configPath string) ([]core.Field, error) {
	if authorization == nil {
		return []core.Field{{Key: "command", Value: append([]string{"nats-server"}, serverArgs...)}}, nil
	}
	script, err := NatsStartScript(*authorization, header, serverArgs, configPath)
	if err != nil {
		return nil, err
	}
	return []core.Field{
		{Key: "entryPoint", Value: []string{"/bin/sh", "-c"}},
		{Key: "command", Value: []string{script}},
	}, nil
}

// NatsStartScript returns the script a NatsService's NATS container runs under `/bin/sh -c` when
// the service has an authorization: it writes the rendered configuration (public keys only) to
// configPath and execs nats-server on it, serverArgs still setting JetStream and the ports. The
// component launches the container with it and `envoy-infra render nats-start-script` prints it.
// Each refusal is a core.InputError naming the component's input.
func NatsStartScript(authorization core.Authorization, configHeader, serverArgs []string, configPath string) (string, error) {
	if err := authorization.Validate(); err != nil {
		return "", core.InputErrorf("authorization", "authorization: %w", err)
	}
	if configPath == "" {
		return "", core.InputErrorf("configPath", "configPath is required with authorization: the start script writes the configuration there")
	}
	script, err := authorization.StartScript(configHeader, serverArgs, configPath)
	if err != nil {
		return "", core.InputErrorf("configHeader", "configHeader: %w", err)
	}
	return script, nil
}

// natsMetricFilters are the nats-server log lines a NatsService counts: each filter's name between
// the prefix and -filter, its pattern and the metric it counts into.
var natsMetricFilters = []struct{ name, pattern, metric string }{
	{"route-disconnect", `"Router connection closed"`, "RouteDisconnections"},
	{"slow-consumer", `"Slow Consumer Detected"`, "SlowConsumers"},
	{"jetstream-error", `?"JetStream out of" ?"JetStream failed to store" ?"JetStream being DISABLED" ?"NO quorum, stalled"`, "JetStreamErrors"},
	// nats-server's own log text for a refused credential and a denied subject.
	{"auth-error", `?"authentication error" ?"Subscription Violation" ?"Publish Violation"`, "AuthErrors"},
}

// NatsMetricFilters returns the log metric filters a NatsService whose
// metricFilterLogicalNamePrefix is prefix and whose metricNamespace is namespace registers, in the
// order it registers them: <prefix>-route-disconnect-filter, <prefix>-slow-consumer-filter,
// <prefix>-jetstream-error-filter and <prefix>-auth-error-filter.
func NatsMetricFilters(prefix, namespace string) []MetricFilter {
	filters := make([]MetricFilter, len(natsMetricFilters))
	for i, f := range natsMetricFilters {
		filters[i] = MetricFilter{
			LogicalName: prefix + "-" + f.name + "-filter", Pattern: f.pattern, MetricName: f.metric, MetricNamespace: namespace,
		}
	}
	return filters
}
