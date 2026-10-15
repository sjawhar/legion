package render_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	envoyaws "github.com/sjawhar/envoy/infra/aws"
	"github.com/sjawhar/envoy/infra/render"
)

const (
	taskDefinitionType = "aws:ecs/taskDefinition:TaskDefinition"
	metricFilterType   = "aws:cloudwatch/logMetricFilter:LogMetricFilter"
	placeholderArn     = "arn:aws:secretsmanager:example-region-1:000000000000:secret:example/"
)

// run runs `envoy-infra render` with args and returns what it wrote and its exit code.
func run(args ...string) (stdout, stderr string, code int) {
	var out, errs bytes.Buffer
	code = render.Command(args, &out, &errs)
	return out.String(), errs.String(), code
}

// registration is one resource the mock engine was asked to register.
type registration struct {
	typ, name string
	inputs    resource.PropertyMap
}

// mocks records every registration and answers with its inputs as its state.
type mocks struct {
	mu         sync.Mutex
	registered []registration
}

func (m *mocks) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.registered = append(m.registered, registration{typ: args.TypeToken, name: args.Name, inputs: args.Inputs})
	return args.Name + "-id", args.Inputs.Copy(), nil
}

func (m *mocks) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	return args.Args, nil
}

// ofType returns every registration of type typ.
func (m *mocks) ofType(typ string) []registration {
	m.mu.Lock()
	defer m.mu.Unlock()
	var found []registration
	for _, r := range m.registered {
		if r.typ == typ {
			found = append(found, r)
		}
	}
	return found
}

// registered runs construct under the mock engine, handing it an aws provider, and returns what
// it registered.
func registered(t *testing.T, construct func(*pulumi.Context, pulumi.ResourceOption) error) *mocks {
	t.Helper()
	m := &mocks{}
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		prov, err := aws.NewProvider(ctx, "example-aws", &aws.ProviderArgs{Region: pulumi.String("example-region-1")})
		if err != nil {
			return err
		}
		return construct(ctx, pulumi.Providers(prov))
	}, pulumi.WithMocks("p", "s", m))
	require.NoError(t, err)
	return m
}

// container returns the container named name from the task definition m registered.
func container(t *testing.T, m *mocks, name string) map[string]any {
	t.Helper()
	tasks := m.ofType(taskDefinitionType)
	require.Len(t, tasks, 1)
	var containers []map[string]any
	require.NoError(t, json.Unmarshal([]byte(tasks[0].inputs["containerDefinitions"].StringValue()), &containers))
	for _, c := range containers {
		if c["name"] == name {
			return c
		}
	}
	require.Failf(t, "no container", "the task definition has no container %q", name)
	return nil
}

// writeFile writes text to a file of its own and returns its path.
func writeFile(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "authorization.json")
	require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
	return path
}

// authorizationJSON is exampleAuthorization as the --authorization-file holds it: the component's
// authorization input, spelled as the schema spells it.
const authorizationJSON = `{
  "users": [
    {
      "name": "example-listener",
      "nkey": "UBERXOSHFHFLL7YEXXH7RYEJLK455TF4BOZLIHD5R6NF5FFISNHEXIZO",
      "publish": {"allow": ["envoy.example.>"], "deny": ["envoy.example.private"]},
      "subscribe": {"allow": ["_INBOX.>", "notifications.café.>"]},
      "hasSeed": true
    },
    {
      "name": "example-daemon",
      "nkey": "UCYF4CJJNU55NFRALQXGPOAZRM5R5JBXI6OAZB2BHWJ4QTTB5XHFENDG",
      "publish": {"allow": ["$JS.API.>"]},
      "subscribe": {"allow": ["_INBOX.>", "legion.work workers"]},
      "hasSeed": true
    },
    {
      "name": "example-fallback",
      "nkey": "UDCQRANOFSHHJXENVBTXNFSRUDIUXGXPNWC5ABJFJ7GBPO27F7UIBVXD",
      "publish": {"allow": [">"]},
      "subscribe": {"allow": [">"]}
    },
    {"name": "example-pending", "hasSeed": true}
  ],
  "noAuthUser": "example-fallback"
}`

// exampleAuthorization is Task 2.1's placeholder authorization, public keys only, as a consumer
// passes it to the component.
func exampleAuthorization() *envoyaws.AuthorizationArgs {
	return &envoyaws.AuthorizationArgs{
		Users: []envoyaws.UserArgs{
			{
				Name:      "example-listener",
				NKey:      "UBERXOSHFHFLL7YEXXH7RYEJLK455TF4BOZLIHD5R6NF5FFISNHEXIZO",
				Publish:   &envoyaws.RulesArgs{Allow: []string{"envoy.example.>"}, Deny: []string{"envoy.example.private"}},
				Subscribe: &envoyaws.RulesArgs{Allow: []string{"_INBOX.>", "notifications.café.>"}},
				HasSeed:   true,
			},
			{
				Name:      "example-daemon",
				NKey:      "UCYF4CJJNU55NFRALQXGPOAZRM5R5JBXI6OAZB2BHWJ4QTTB5XHFENDG",
				Publish:   &envoyaws.RulesArgs{Allow: []string{"$JS.API.>"}},
				Subscribe: &envoyaws.RulesArgs{Allow: []string{"_INBOX.>", "legion.work workers"}},
				HasSeed:   true,
			},
			{
				Name:      "example-fallback",
				NKey:      "UDCQRANOFSHHJXENVBTXNFSRUDIUXGXPNWC5ABJFJ7GBPO27F7UIBVXD",
				Publish:   &envoyaws.RulesArgs{Allow: []string{">"}},
				Subscribe: &envoyaws.RulesArgs{Allow: []string{">"}},
			},
			{Name: "example-pending", HasSeed: true},
		},
		NoAuthUser: "example-fallback",
	}
}

// natsArgs are placeholder inputs of a NatsService.
func natsArgs() *envoyaws.NatsServiceArgs {
	return &envoyaws.NatsServiceArgs{
		Authorization: exampleAuthorization(),
		ConfigHeader:  []string{"Rendered by envoy-infra from placeholder inputs.", "Public keys only."},
		ServerArgs:    []string{"--jetstream", "--store_dir", "/data", "--port", "4222", "--http_port", "8222"},
		ConfigPath:    "/tmp/nats-server.conf",

		NatsImage:              "example.registry/nats:placeholder",
		TailscaleImage:         "example.registry/tailscale@sha256:" + strings.Repeat("0", 64),
		TailnetHostname:        "example-bus",
		TailnetTags:            []string{"tag:example-a"},
		TailnetClientSecretArn: pulumi.String(placeholderArn + "tailnet-oauth"),

		Region:           "example-region-1",
		VpcId:            pulumi.String("example-vpc"),
		SubnetIds:        []pulumi.StringInput{pulumi.String("example-subnet-a")},
		SecurityGroupId:  pulumi.String("example-nats-sg"),
		ClusterArn:       pulumi.String("example-cluster-arn"),
		ExecutionRoleArn: pulumi.String("example-execution-role-arn"),
		TaskRoleArn:      pulumi.String("example-task-role-arn"),

		CloudMapNamespaceId:        pulumi.String("example-namespace-id"),
		CloudMapNamespaceName:      pulumi.String("example-namespace.internal.example"),
		CloudMapServiceName:        "example-bus",
		CloudMapServiceLogicalName: "example-nats-cloudmap",

		Family:           "example-nats-family",
		LogGroupName:     "/ecs/example-nats",
		LogRetentionDays: 7,
		Cpu:              "256",
		Memory:           "1024",
		PlatformVersion:  "LATEST",

		BaseTags:                      map[string]string{"project": "example-project"},
		MetricNamespace:               "example/NATS",
		MetricFilterLogicalNamePrefix: "example-bus",
	}
}

// listenerArgs are placeholder inputs of a WebhookListener.
func listenerArgs() *envoyaws.WebhookListenerArgs {
	return &envoyaws.WebhookListenerArgs{
		MachineId: "example-machine",
		NatsUrls:  pulumi.ToStringArray([]string{"nats://example-bus.example-namespace.internal.example:4222"}),
		Image:     "example.registry/envoy@sha256:" + strings.Repeat("0", 64),
		Region:    "example-region-1",

		ApiTokenSecretArn:       pulumi.String(placeholderArn + "api-token"),
		ApiTokenSecretVersionId: pulumi.String("example-token-version"),
		SeedSecretArn:           pulumi.String(placeholderArn + "nats-seed"),
		Webhooks:                []string{"github", "slack"},
		GitHubWebhookSecretArn:  pulumi.String(placeholderArn + "github-webhook"),
		SlackSigningSecretArn:   pulumi.String(placeholderArn + "slack-signing"),
		ReviewerAppId:           pulumi.StringRef("1234567"),

		ClusterArn:        pulumi.String("example-cluster-arn"),
		ExecutionRoleArn:  pulumi.String("example-execution-role-arn"),
		ExecutionRoleName: pulumi.String("example-execution-role"),
		TaskRoleArn:       pulumi.String("example-task-role-arn"),
		SubnetIds:         []pulumi.StringInput{pulumi.String("example-subnet-a")},
		SecurityGroupId:   pulumi.String("example-listener-sg"),
		TargetGroupArn:    pulumi.String("example-target-group-arn"),

		CloudMapNamespaceId:        pulumi.String("example-namespace-id"),
		CloudMapNamespaceName:      pulumi.String("example-namespace.internal.example"),
		CloudMapServiceName:        "example-hooks",
		CloudMapServiceLogicalName: "example-listener-cloudmap",

		Family:           "example-listener-family",
		LogGroupName:     "/ecs/example-listener",
		LogRetentionDays: 7,
		Cpu:              "256",
		Memory:           "512",

		ListenHost:     "0.0.0.0",
		HostBridge:     "127.0.0.1",
		NatsReplicas:   1,
		MentionTrigger: pulumi.StringRef("@legion"),

		AlarmTopicArn:   pulumi.String("arn:aws:sns:example-region-1:000000000000:example-alarms"),
		BaseTags:        map[string]string{"project": "example-project"},
		MetricNamespace: "example/EnvoyListener",
		NatsServiceArn:  pulumi.String("example-nats-service-arn"),
	}
}

// flags returns the flags that set each value, a repeatable flag once per value.
func flags(name string, values ...string) []string {
	var args []string
	for _, v := range values {
		args = append(args, "--"+name, v)
	}
	return args
}

// The script is the NATS container's command as NatsService registers it, byte for byte: no
// newline follows it, since the container runs the registered string itself.
func TestNatsStartScriptIsTheScriptNatsServiceLaunches(t *testing.T) {
	for _, c := range []struct {
		name          string
		file          string
		authorization *envoyaws.AuthorizationArgs
		header        []string
		serverArgs    []string
		configPath    string
	}{
		{
			name:          "header lines, server arguments and a fallback user",
			file:          authorizationJSON,
			authorization: exampleAuthorization(),
			header:        []string{"Rendered by envoy-infra from placeholder inputs.", "Public keys only."},
			serverArgs:    []string{"--jetstream", "--store_dir", "/data", "--port", "4222", "--http_port", "8222"},
			configPath:    "/tmp/nats-server.conf",
		},
		{
			name: "no header line, one server argument, no fallback user",
			file: `{"users": [{"name": "example-host-a", "nkey": "UBERXOSHFHFLL7YEXXH7RYEJLK455TF4BOZLIHD5R6NF5FFISNHEXIZO", ` +
				`"publish": {"allow": [">"]}, "subscribe": {"allow": [">"]}, "hasSeed": true}]}`,
			authorization: &envoyaws.AuthorizationArgs{Users: []envoyaws.UserArgs{{
				Name:      "example-host-a",
				NKey:      "UBERXOSHFHFLL7YEXXH7RYEJLK455TF4BOZLIHD5R6NF5FFISNHEXIZO",
				Publish:   &envoyaws.RulesArgs{Allow: []string{">"}},
				Subscribe: &envoyaws.RulesArgs{Allow: []string{">"}},
				HasSeed:   true,
			}}},
			serverArgs: []string{"--jetstream"},
			configPath: "/etc/nats/example server.conf",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			args := append([]string{"nats-start-script", "--authorization-file", writeFile(t, c.file)}, flags("config-header", c.header...)...)
			args = append(append(args, flags("server-arg", c.serverArgs...)...), "--config-path", c.configPath)
			stdout, stderr, code := run(args...)
			require.Equal(t, 0, code, stderr)
			require.Empty(t, stderr)

			in := natsArgs()
			in.Authorization, in.ConfigHeader, in.ServerArgs, in.ConfigPath = c.authorization, c.header, c.serverArgs, c.configPath
			m := registered(t, func(ctx *pulumi.Context, opts pulumi.ResourceOption) error {
				_, err := envoyaws.NewNatsService(ctx, "example-nats", in, opts)
				return err
			})
			nats := container(t, m, "nats")
			require.Equal(t, []any{"/bin/sh", "-c"}, nats["entryPoint"])
			require.Equal(t, []any{stdout}, nats["command"])
		})
	}
}

// The lines are the listener container's environment as WebhookListener registers it, in its
// order. The secrets ECS injects are in the container's secrets, not its environment, so none is
// printed.
func TestWebhookListenerEnvIsTheEnvironmentWebhookListenerRegisters(t *testing.T) {
	for _, c := range []struct {
		name                   string
		natsUrls               []string
		listenHost, hostBridge string
		replicas               int
		webhooks               []string
		trigger, appID         string // given with github
		want                   string // derived by hand from the listener's environment rules
	}{
		{
			name:       "a remote NATS server and a host bridge",
			natsUrls:   []string{"nats://example-bus.example-namespace.internal.example:4222"},
			listenHost: "0.0.0.0", hostBridge: "127.0.0.1", replicas: 1,
			webhooks: []string{"github", "slack"}, trigger: "@legion", appID: "1234567",
			want: "PORT=9020\n" +
				"ENVOY_LISTEN_HOST=0.0.0.0\n" +
				"ENVOY_MACHINE_ID=example-machine\n" +
				"NATS_URLS=nats://example-bus.example-namespace.internal.example:4222\n" +
				"ENVOY_ALLOW_REMOTE_NATS=1\n" +
				"ENVOY_HOST_BRIDGE=127.0.0.1\n" +
				"ENVOY_NATS_REPLICAS=1\n" +
				"ENVOY_WEBHOOKS=github,slack\n" +
				"ENVOY_GITHUB_MENTION_TRIGGER=@legion\n" +
				"ENVOY_REVIEWER_APP_ID=1234567\n",
		},
		{
			name:       "two loopback NATS servers and no host bridge",
			natsUrls:   []string{"nats://127.0.0.1:4222", "nats://localhost:4223"},
			listenHost: "::", hostBridge: "", replicas: 3,
			webhooks: []string{"github", "slack"}, trigger: "@example-bot", appID: "7654321",
			want: "PORT=9020\n" +
				"ENVOY_LISTEN_HOST=::\n" +
				"ENVOY_MACHINE_ID=example-machine\n" +
				"NATS_URLS=nats://127.0.0.1:4222,nats://localhost:4223\n" +
				"ENVOY_NATS_REPLICAS=3\n" +
				"ENVOY_WEBHOOKS=github,slack\n" +
				"ENVOY_GITHUB_MENTION_TRIGGER=@example-bot\n" +
				"ENVOY_REVIEWER_APP_ID=7654321\n",
		},
		{
			name:       "no webhook",
			natsUrls:   []string{"nats://example-bus.example-namespace.internal.example:4222"},
			listenHost: "0.0.0.0", hostBridge: "127.0.0.1", replicas: 1,
			webhooks: []string{},
			want: "PORT=9020\n" +
				"ENVOY_LISTEN_HOST=0.0.0.0\n" +
				"ENVOY_MACHINE_ID=example-machine\n" +
				"NATS_URLS=nats://example-bus.example-namespace.internal.example:4222\n" +
				"ENVOY_ALLOW_REMOTE_NATS=1\n" +
				"ENVOY_HOST_BRIDGE=127.0.0.1\n" +
				"ENVOY_NATS_REPLICAS=1\n",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			github := slices.Contains(c.webhooks, "github")
			args := append([]string{"webhook-listener-env", "--machine-id", "example-machine"}, flags("nats-url", c.natsUrls...)...)
			args = append(args, flags("webhook", c.webhooks...)...)
			if github {
				args = append(args, "--reviewer-app-id", c.appID, "--mention-trigger", c.trigger)
			}
			args = append(args, "--listen-host", c.listenHost, "--host-bridge", c.hostBridge,
				"--nats-replicas", strconv.Itoa(c.replicas))
			stdout, stderr, code := run(args...)
			require.Equal(t, 0, code, stderr)
			require.Empty(t, stderr)
			require.Equal(t, c.want, stdout)

			in := listenerArgs()
			in.NatsUrls = pulumi.ToStringArray(c.natsUrls)
			in.ListenHost, in.HostBridge, in.NatsReplicas = c.listenHost, c.hostBridge, c.replicas
			in.Webhooks = c.webhooks
			in.MentionTrigger, in.ReviewerAppId = pulumi.StringRef(c.trigger), pulumi.StringRef(c.appID)
			if !github {
				in.GitHubWebhookSecretArn, in.MentionTrigger, in.ReviewerAppId = nil, nil, nil
			}
			if !slices.Contains(c.webhooks, "slack") {
				in.SlackSigningSecretArn = nil
			}
			m := registered(t, func(ctx *pulumi.Context, opts pulumi.ResourceOption) error {
				_, err := envoyaws.NewWebhookListener(ctx, "example-listener", in, opts)
				return err
			})
			var env strings.Builder
			for _, v := range container(t, m, "envoy-listener")["environment"].([]any) {
				variable := v.(map[string]any)
				env.WriteString(variable["name"].(string) + "=" + variable["value"].(string) + "\n")
			}
			require.Equal(t, env.String(), stdout)
		})
	}
}

// filter is one entry of `render metric-filters --json`.
type filter struct {
	LogicalName     string `json:"logicalName"`
	Pattern         string `json:"pattern"`
	MetricName      string `json:"metricName"`
	MetricNamespace string `json:"metricNamespace"`
}

// The filters are the log metric filters each component registers, by logical name: a
// NatsService names its filters from metricFilterLogicalNamePrefix, a WebhookListener from its
// own name.
func TestMetricFiltersAreTheFiltersEachComponentRegisters(t *testing.T) {
	for _, c := range []struct {
		component, prefix, namespace string
		count                        int
		construct                    func(*pulumi.Context, pulumi.ResourceOption) error
	}{
		{
			component: "nats-service", prefix: "example-bus", namespace: "example/NATS", count: 4,
			construct: func(ctx *pulumi.Context, opts pulumi.ResourceOption) error {
				in := natsArgs()
				in.MetricFilterLogicalNamePrefix, in.MetricNamespace = "example-bus", "example/NATS"
				_, err := envoyaws.NewNatsService(ctx, "example-nats", in, opts)
				return err
			},
		},
		{
			component: "webhook-listener", prefix: "example-hooks", namespace: "example/EnvoyListener", count: 8,
			construct: func(ctx *pulumi.Context, opts pulumi.ResourceOption) error {
				in := listenerArgs()
				in.MetricNamespace = "example/EnvoyListener"
				_, err := envoyaws.NewWebhookListener(ctx, "example-hooks", in, opts)
				return err
			},
		},
	} {
		t.Run(c.component, func(t *testing.T) {
			stdout, stderr, code := run("metric-filters", "--component", c.component,
				"--metric-namespace", c.namespace, "--logical-name-prefix", c.prefix, "--json")
			require.Equal(t, 0, code, stderr)
			require.Empty(t, stderr)
			decoder := json.NewDecoder(strings.NewReader(stdout))
			decoder.DisallowUnknownFields()
			var rendered []filter
			require.NoError(t, decoder.Decode(&rendered))
			require.False(t, decoder.More(), "one JSON value")

			var want []filter
			for _, r := range registered(t, c.construct).ofType(metricFilterType) {
				transformation := r.inputs["metricTransformation"].ObjectValue()
				want = append(want, filter{
					LogicalName:     r.name,
					Pattern:         r.inputs["pattern"].StringValue(),
					MetricName:      transformation["name"].StringValue(),
					MetricNamespace: transformation["namespace"].StringValue(),
				})
			}
			require.Len(t, want, c.count)
			// The mock engine records registrations as each one's inputs resolve, in no fixed order.
			require.ElementsMatch(t, want, rendered)
			for _, f := range rendered {
				require.True(t, strings.HasPrefix(f.LogicalName, c.prefix+"-"), f.LogicalName)
			}
		})
	}
}

// refusal is one invocation the command refuses: exit 2, nothing on stdout, and want on stderr.
type refusal struct {
	name  string
	args  []string
	want  string
	usage bool // a usage error, which prints the usage after the problem
}

func requireRefused(t *testing.T, rows []refusal) {
	t.Helper()
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			stdout, stderr, code := run(row.args...)
			require.Equal(t, 2, code, stderr)
			require.Empty(t, stdout)
			require.Contains(t, stderr, row.want)
			require.Equal(t, row.usage, strings.Contains(stderr, "usage: envoy-infra render"), "the usage is printed exactly on a usage error: %q", stderr)
		})
	}
}

func TestNatsStartScriptRefusals(t *testing.T) {
	valid := writeFile(t, authorizationJSON)
	with := func(file string, rest ...string) []string {
		return append([]string{"nats-start-script", "--authorization-file", file, "--config-path", "/tmp/nats-server.conf"}, rest...)
	}
	requireRefused(t, []refusal{
		{
			name: "no --authorization-file", usage: true,
			args: []string{"nats-start-script", "--config-path", "/tmp/nats-server.conf"},
			want: "envoy-infra render nats-start-script: --authorization-file is required",
		},
		{
			name: "no --config-path", usage: true,
			args: []string{"nats-start-script", "--authorization-file", valid},
			want: "envoy-infra render nats-start-script: --config-path is required",
		},
		{
			name: "an argument after the flags", usage: true,
			args: with(valid, "extra"),
			want: "envoy-infra render nats-start-script: unexpected arguments after the flags",
		},
		{
			name: "an --authorization-file that does not exist",
			args: with(filepath.Join(t.TempDir(), "absent.json")),
			want: "refused: --authorization-file: open ",
		},
		{
			name: "an --authorization-file that is not JSON",
			args: with(writeFile(t, "users: []")),
			want: "refused: --authorization-file: ",
		},
		{
			name: "an --authorization-file with a field the input does not have",
			args: with(writeFile(t, `{"users": [{"name": "example-host-a", "seed": "x"}]}`)),
			want: `refused: --authorization-file: json: unknown field "seed"`,
		},
		// Keys are spelled as the schema spells them; encoding/json alone would match any case.
		{
			name: "an --authorization-file with a top-level key in another case",
			args: with(writeFile(t, strings.Replace(authorizationJSON, `"users"`, `"USERS"`, 1))),
			want: `refused: --authorization-file: key "USERS" is spelled "users" in the schema`,
		},
		{
			name: "an --authorization-file with a user's key in another case",
			args: with(writeFile(t, strings.Replace(authorizationJSON, `"nkey"`, `"NKey"`, 1))),
			want: `refused: --authorization-file: users[0]: key "NKey" is spelled "nkey" in the schema`,
		},
		{
			name: "an --authorization-file with a rule's key in another case",
			args: with(writeFile(t, strings.Replace(authorizationJSON, `"allow"`, `"Allow"`, 1))),
			want: `refused: --authorization-file: users[0].publish: key "Allow" is spelled "allow" in the schema`,
		},
		{
			name: "an --authorization-file holding two values",
			args: with(writeFile(t, authorizationJSON+"\n{}")),
			want: "refused: --authorization-file: holds something after its JSON value",
		},
		{
			name: "an --authorization-file with a stray bracket after its value",
			args: with(writeFile(t, authorizationJSON+"]")),
			want: "refused: --authorization-file: holds something after its JSON value",
		},
		{
			name: "an authorization the component refuses",
			args: with(writeFile(t, `{"users": []}`)),
			want: "refused: --authorization-file: authorization: users: an authorization needs at least one user",
		},
		{
			name: "an empty --config-path",
			args: []string{"nats-start-script", "--authorization-file", valid, "--config-path", ""},
			want: "refused: --config-path: configPath is required with authorization",
		},
		{
			name: "a --config-header line holding a line break",
			args: with(valid, "--config-header", "one\ntwo"),
			want: "refused: --config-header: configHeader: header[0] holds a line break",
		},
		{
			name: "a --server-arg that sets a password beside the rendered users",
			args: with(valid, "--server-arg", "--jetstream", "--server-arg", "--pass=example"),
			want: "refused: --server-arg: serverArgs[1] sets nats-server's -pass, which sets a password beside the rendered nkey users",
		},
	})
}

// listenerFlags are a valid invocation of webhook-listener-env with the flags drop names removed
// and set's flags appended.
func listenerFlags(drop []string, set ...string) []string {
	given := map[string][]string{
		"machine-id":      {"example-machine"},
		"nats-url":        {"nats://example-bus.example-namespace.internal.example:4222"},
		"webhook":         {"github", "slack"},
		"reviewer-app-id": {"1234567"},
		"mention-trigger": {"@legion"},
		"listen-host":     {"0.0.0.0"},
		"host-bridge":     {"127.0.0.1"},
		"nats-replicas":   {"1"},
	}
	args := []string{"webhook-listener-env"}
	for _, name := range []string{"machine-id", "nats-url", "webhook", "reviewer-app-id", "mention-trigger", "listen-host", "host-bridge", "nats-replicas"} {
		if !slices.Contains(drop, name) {
			args = append(args, flags(name, given[name]...)...)
		}
	}
	return append(args, set...)
}

func TestWebhookListenerEnvRefusals(t *testing.T) {
	requireRefused(t, []refusal{
		// The environment, as the component refuses it. rendering.ListenerEnv.Check's refusals are
		// tested in rendering and core.ListenerConfig's in core; one row per input they refuse
		// shows the flag that input maps to.
		{
			name: "--webhook github alone",
			args: listenerFlags([]string{"webhook"}, "--webhook", "github"),
			want: `refused: --webhook: webhooks ["github"] is not supported: this version takes [] or ["github" "slack"]`,
		},
		{
			name: "--webhook github without --reviewer-app-id",
			args: listenerFlags([]string{"reviewer-app-id"}),
			want: `refused: --reviewer-app-id: reviewerAppId is required with webhook "github"`,
		},
		{
			name: "--webhook github without --mention-trigger",
			args: listenerFlags([]string{"mention-trigger"}),
			want: `refused: --mention-trigger: mentionTrigger is required with webhook "github"`,
		},
		{
			name: "a blank --machine-id",
			args: listenerFlags([]string{"machine-id"}, "--machine-id", " "),
			want: "refused: --machine-id: machineId is required",
		},
		{
			name: "a line break in --host-bridge",
			args: listenerFlags([]string{"host-bridge"}, "--host-bridge", "127.0\n.0.1"),
			want: `refused: --host-bridge: hostBridge contains a control character: "127.0\n.0.1"`,
		},
		{
			name: "a blank second --nats-url",
			args: listenerFlags(nil, "--nats-url", " "),
			want: "refused: --nats-url: natsUrls[1] is empty",
		},
		{
			name: "a blank --listen-host",
			args: listenerFlags([]string{"listen-host"}, "--listen-host", " "),
			want: "refused: --listen-host: listenHost is required",
		},
		{
			name: "--nats-replicas 0",
			args: listenerFlags([]string{"nats-replicas"}, "--nats-replicas", "0"),
			want: "refused: --nats-replicas: natsReplicas 0 is below 1",
		},
		// A value no KEY=value line can carry, which the component would write into its JSON.
		{
			name: "a line break in --listen-host",
			args: listenerFlags([]string{"listen-host"}, "--listen-host", "0.0.0.0\n"),
			want: "refused: --listen-host: holds a line break, which a KEY=value line cannot carry",
		},
		{
			name: "a line break in --mention-trigger",
			args: listenerFlags([]string{"mention-trigger"}, "--mention-trigger", "@leg\nion"),
			want: "refused: --mention-trigger: holds a line break, which a KEY=value line cannot carry",
		},
		{
			name: "a line break in --reviewer-app-id",
			args: listenerFlags([]string{"reviewer-app-id"}, "--reviewer-app-id", "123\n4567"),
			want: "refused: --reviewer-app-id: holds a line break, which a KEY=value line cannot carry",
		},
		// Usage errors.
		{
			name: "no --machine-id", usage: true,
			args: listenerFlags([]string{"machine-id"}),
			want: "envoy-infra render webhook-listener-env: --machine-id is required",
		},
		{
			name: "no --listen-host", usage: true,
			args: listenerFlags([]string{"listen-host"}),
			want: "envoy-infra render webhook-listener-env: --listen-host is required",
		},
		{
			name: "no --host-bridge", usage: true,
			args: listenerFlags([]string{"host-bridge"}),
			want: "envoy-infra render webhook-listener-env: --host-bridge is required",
		},
		{
			name: "no --nats-replicas", usage: true,
			args: listenerFlags([]string{"nats-replicas"}),
			want: "envoy-infra render webhook-listener-env: --nats-replicas is required",
		},
		{
			name: "a --nats-replicas that is not a number", usage: true,
			args: listenerFlags([]string{"nats-replicas"}, "--nats-replicas", "two"),
			want: `invalid value "two" for flag -nats-replicas`,
		},
		{
			name: "an argument after the flags", usage: true,
			args: listenerFlags(nil, "extra"),
			want: "envoy-infra render webhook-listener-env: unexpected arguments after the flags",
		},
	})
}

func TestMetricFiltersRefusals(t *testing.T) {
	with := func(drop string, rest ...string) []string {
		args := []string{"metric-filters"}
		for _, f := range [][]string{
			{"--component", "nats-service"}, {"--metric-namespace", "example/NATS"}, {"--logical-name-prefix", "example-bus"}, {"--json"},
		} {
			if f[0] != drop {
				args = append(args, f...)
			}
		}
		return append(args, rest...)
	}
	requireRefused(t, []refusal{
		{name: "no --component", usage: true, args: with("--component"), want: "envoy-infra render metric-filters: --component is required"},
		{
			name: "a component that has no filters", usage: true,
			args: with("--component", "--component", "host-listener"),
			want: "envoy-infra render metric-filters: --component is nats-service or webhook-listener",
		},
		{name: "no --metric-namespace", usage: true, args: with("--metric-namespace"), want: "envoy-infra render metric-filters: --metric-namespace is required"},
		{name: "no --logical-name-prefix", usage: true, args: with("--logical-name-prefix"), want: "envoy-infra render metric-filters: --logical-name-prefix is required"},
		{name: "no --json", usage: true, args: with("--json"), want: "envoy-infra render metric-filters: --json is required"},
		{name: "an argument after the flags", usage: true, args: with("", "extra"), want: "envoy-infra render metric-filters: unexpected arguments after the flags"},
	})
}

func TestRenderUsage(t *testing.T) {
	requireRefused(t, []refusal{
		{name: "no subcommand", usage: true, args: nil, want: "envoy-infra render: a subcommand is required"},
		{
			name: "a subcommand render does not have", usage: true, args: []string{"seed"},
			want: "envoy-infra render: the subcommand is nats-start-script, webhook-listener-env or metric-filters",
		},
	})
	for _, args := range [][]string{
		{"--help"}, {"nats-start-script", "--help"}, {"webhook-listener-env", "-h"}, {"metric-filters", "--help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			stdout, stderr, code := run(args...)
			require.Equal(t, 0, code)
			require.Empty(t, stdout)
			require.Contains(t, stderr, "usage: envoy-infra render")
		})
	}
}
