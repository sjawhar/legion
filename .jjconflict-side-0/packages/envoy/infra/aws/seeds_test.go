package aws_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	envoyaws "github.com/sjawhar/envoy/infra/aws"
	"github.com/sjawhar/envoy/infra/internal/pulumitest"
)

// testSeed is nats-io's vector user seed (nkeys v0.4.15 README.md line 45); testPublicKey is its
// public key (line 49), a user key no example uses.
const (
	testSeed      = "SUAKYRHVIOREXV7EUZTBHUHL7NUMHPMAS7QMDU3GTIUWEI5LDNOXD43IZY"
	testPublicKey = "UD466L6EBCM3YY5HEGHJANNTN4LSKTSUXTH7RILHCKEQMQHTBNLHJJXT"
)

// lowerSeed and splitSeed are testSeed spelled as a reader would still read it: lowercase, and cut
// in two by a separator.
var lowerSeed = strings.ToLower(testSeed)

func splitSeed(sep string) string { return testSeed[:29] + sep + testSeed[29:] }

// holdsTestSeed reports whether text holds twelve letters of testSeed in a row, in either case and
// with '.', '-', '_', '/' and whitespace removed, as a reader joins a seed split over them.
func holdsTestSeed(text string) bool {
	joined := strings.Map(func(r rune) rune {
		if strings.ContainsRune(".-_/ \t\r\n", r) {
			return -1
		}
		return unicode.ToUpper(r)
	}, text)
	for i := range len(testSeed) - 11 {
		if strings.Contains(joined, testSeed[i:i+12]) {
			return true
		}
	}
	return false
}

// carried returns every string a run carries: each registration's name, its aliases, and each
// string of its inputs, map keys included and secrets and outputs unwrapped, and each string of
// the component's outputs.
func carried(m *pulumitest.Mocks, state property.Map) []string {
	var out []string
	var walk func(v resource.PropertyValue)
	walk = func(v resource.PropertyValue) {
		switch {
		case v.IsSecret():
			walk(v.SecretValue().Element)
		case v.IsOutput():
			walk(v.OutputValue().Element)
		case v.IsString():
			out = append(out, v.StringValue())
		case v.IsArray():
			for _, e := range v.ArrayValue() {
				walk(e)
			}
		case v.IsObject():
			for k, e := range v.ObjectValue() {
				out = append(out, string(k))
				walk(e)
			}
		}
	}
	for _, r := range m.Registrations() {
		out = append(out, r.Name)
		for _, a := range r.RPC.GetAliases() {
			out = append(out, a.String())
		}
		walk(resource.NewObjectProperty(r.Inputs))
	}
	for _, v := range state.All {
		if v.IsString() {
			out = append(out, v.AsString())
		}
	}
	return out
}

// seedRow is one string input of a component: set puts a value holding testSeed there, in some
// spelling, when seeded is set, and otherwise a clean value of the same shape the component
// accepts, and returns the value it put. orders marks an input that only orders a child after its
// resource, so no registration carries its value.
type seedRow[A any] struct {
	input  string
	set    func(a *A, seeded bool) string
	orders bool
}

// pick returns seed or clean.
func pick(seeded bool, seed, clean string) string {
	if seeded {
		return seed
	}
	return clean
}

// requireSeedRefused constructs the component named name, through the plugin's Construct, from
// each row's args with the input holding a seed, and requires the run to fail naming the input by
// its path, with no registration, output or error carrying the seed in any spelling. It then
// constructs it with the row's clean value, which succeeds and, unless the input only orders,
// reaches what the run carries.
func requireSeedRefused[A any](t *testing.T, typ, name string, rows []seedRow[A], example func() *A,
	in func(t *testing.T, a *A) map[string]property.Value,
) {
	t.Helper()
	for _, row := range rows {
		t.Run(row.input, func(t *testing.T) {
			args := example()
			row.set(args, true)
			m, resp, err := construct(t, typ, name, in(t, args), false)
			require.ErrorContains(t, err, typ+" "+name+": "+row.input+" holds an nkey seed (not echoed)")
			require.False(t, holdsTestSeed(err.Error()), "the refusal quotes the seed: %v", err)
			for _, s := range carried(m, resp.State) {
				require.False(t, holdsTestSeed(s), "the run carries the seed: %q", s)
			}

			args = example()
			clean := row.set(args, false)
			m, resp, err = construct(t, typ, name, in(t, args), false)
			require.NoError(t, err)
			if !row.orders {
				require.True(t, slices.ContainsFunc(carried(m, resp.State), func(s string) bool { return strings.Contains(s, clean) }),
					"the clean value %q reaches the run", clean)
			}
		})
	}
}

// requireNameRefused requires a component named with a seed, in each spelling, to be refused
// naming its name and never the value, with nothing carrying it; a clean name constructs.
func requireNameRefused(t *testing.T, typ string, in func(t *testing.T) map[string]property.Value) {
	t.Helper()
	for _, name := range []string{"example-" + testSeed, lowerSeed, splitSeed("-")} {
		t.Run("the component's name", func(t *testing.T) {
			m, resp, err := construct(t, typ, name, in(t), false)
			require.ErrorContains(t, err, typ+": the component's name holds an nkey seed (not echoed)")
			require.False(t, holdsTestSeed(err.Error()), "the refusal quotes the seed: %v", err)
			for _, s := range carried(m, resp.State) {
				require.False(t, holdsTestSeed(s), "the run carries the seed: %q", s)
			}
		})
	}
	t.Run("the component's name, clean", func(t *testing.T) {
		m, resp, err := construct(t, typ, "example-other-name", in(t), false)
		require.NoError(t, err)
		require.True(t, slices.ContainsFunc(carried(m, resp.State), func(s string) bool { return strings.Contains(s, "example-other-name") }))
	})
}

func natsSeedRows() []seedRow[envoyaws.NatsServiceArgs] {
	type A = envoyaws.NatsServiceArgs
	return []seedRow[A]{
		{input: "authorization.users[1].name", set: func(a *A, s bool) string {
			a.Authorization.Users[1].Name = pick(s, testSeed, "example-other")
			return a.Authorization.Users[1].Name
		}},
		{input: "authorization.users[0].nkey", set: func(a *A, s bool) string {
			a.Authorization.Users[0].NKey = pick(s, testSeed, testPublicKey)
			return a.Authorization.Users[0].NKey
		}},
		{input: "authorization.users[0].publish.allow[0]", set: func(a *A, s bool) string {
			a.Authorization.Users[0].Publish.Allow[0] = "envoy." + pick(s, lowerSeed, "other") + ".>"
			return a.Authorization.Users[0].Publish.Allow[0]
		}},
		{input: "authorization.users[0].publish.deny[0]", set: func(a *A, s bool) string {
			a.Authorization.Users[0].Publish.Deny[0] = "envoy." + pick(s, testSeed, "secret")
			return a.Authorization.Users[0].Publish.Deny[0]
		}},
		{input: "authorization.users[0].subscribe.allow[0]", set: func(a *A, s bool) string {
			a.Authorization.Users[0].Subscribe.Allow[0] = "_INBOX." + pick(s, splitSeed("."), "other.label")
			return a.Authorization.Users[0].Subscribe.Allow[0]
		}},
		{input: "authorization.users[1].subscribe.allow[1]", set: func(a *A, s bool) string {
			a.Authorization.Users[1].Subscribe.Allow[1] = "legion.work " + pick(s, testSeed, "crew")
			return a.Authorization.Users[1].Subscribe.Allow[1]
		}},
		{input: "authorization.noAuthUser", set: func(a *A, s bool) string {
			a.Authorization.NoAuthUser = pick(s, lowerSeed, "example-fallback")
			return a.Authorization.NoAuthUser
		}},
		{input: "configHeader[1]", set: func(a *A, s bool) string {
			a.ConfigHeader[1] = "Public keys only, " + pick(s, splitSeed(" "), "today")
			return a.ConfigHeader[1]
		}},
		{input: "serverArgs[2]", set: func(a *A, s bool) string {
			a.ServerArgs[2] = "/data/" + pick(s, testSeed, "jetstream")
			return a.ServerArgs[2]
		}},
		{input: "configPath", set: func(a *A, s bool) string {
			a.ConfigPath = pulumi.StringRef("/tmp/" + pick(s, lowerSeed, "nats-other") + ".conf")
			return *a.ConfigPath
		}},
		{input: "natsImage", set: func(a *A, s bool) string {
			a.NatsImage = "example.registry/" + pick(s, lowerSeed, "nats-other") + ":placeholder"
			return a.NatsImage
		}},
		{input: "tailscaleImage", set: func(a *A, s bool) string {
			a.TailscaleImage = "example.registry/" + pick(s, lowerSeed, "tailscale-other") + ":placeholder"
			return a.TailscaleImage
		}},
		{input: "tailnetHostname", set: func(a *A, s bool) string {
			a.TailnetHostname = pick(s, splitSeed("-"), "envoy-other")
			return a.TailnetHostname
		}},
		{input: "tailnetTags[1]", set: func(a *A, s bool) string {
			a.TailnetTags[1] = "tag:" + pick(s, lowerSeed, "example-other")
			return a.TailnetTags[1]
		}},
		{input: "region", set: func(a *A, s bool) string {
			a.Region = pick(s, testSeed, "example-region-2")
			return a.Region
		}},
		{input: "cloudMapServiceName", set: func(a *A, s bool) string {
			a.CloudMapServiceName = pick(s, lowerSeed, "example-other-bus")
			return a.CloudMapServiceName
		}},
		{input: "cloudMapServiceLogicalName", set: func(a *A, s bool) string {
			a.CloudMapServiceLogicalName = pick(s, testSeed, "example-other-cloudmap")
			return a.CloudMapServiceLogicalName
		}},
		{input: "family", set: func(a *A, s bool) string {
			a.Family = pick(s, testSeed, "example-other-family")
			return a.Family
		}},
		{input: "logGroupName", set: func(a *A, s bool) string {
			a.LogGroupName = "/ecs/" + pick(s, splitSeed("_"), "nats-other")
			return a.LogGroupName
		}},
		{input: "cpu", set: func(a *A, s bool) string { a.Cpu = pick(s, testSeed, "512"); return a.Cpu }},
		{input: "memory", set: func(a *A, s bool) string { a.Memory = pick(s, testSeed, "2048"); return a.Memory }},
		{input: "platformVersion", set: func(a *A, s bool) string {
			a.PlatformVersion = pick(s, testSeed, "1.4.0")
			return a.PlatformVersion
		}},
		{input: "baseTags[env]", set: func(a *A, s bool) string {
			a.BaseTags["env"] = pick(s, lowerSeed, "example-other-env")
			return a.BaseTags["env"]
		}},
		{input: "a key of baseTags", set: func(a *A, s bool) string {
			key := pick(s, testSeed, "owner")
			a.BaseTags[key] = "example-owner"
			return key
		}},
		{input: "metricNamespace", set: func(a *A, s bool) string {
			a.MetricNamespace = "example/" + pick(s, testSeed, "NATS-other")
			return a.MetricNamespace
		}},
		{input: "metricFilterLogicalNamePrefix", set: func(a *A, s bool) string {
			a.MetricFilterLogicalNamePrefix = pick(s, splitSeed("-"), "example-other-bus")
			return a.MetricFilterLogicalNamePrefix
		}},
		// Inputs, refused once they resolve.
		{input: "tailnetClientSecretArn", set: func(a *A, s bool) string {
			v := secretArn + pick(s, testSeed, "-other")
			a.TailnetClientSecretArn = pulumi.String(v)
			return v
		}},
		{input: "vpcId", set: func(a *A, s bool) string {
			v := "vpc-" + pick(s, lowerSeed, "other")
			a.VpcId = pulumi.String(v)
			return v
		}},
		{input: "subnetIds[1]", set: func(a *A, s bool) string {
			v := "subnet-" + pick(s, lowerSeed, "other")
			a.SubnetIds[1] = pulumi.String(v)
			return v
		}},
		{input: "securityGroupId", set: func(a *A, s bool) string {
			v := "sg-" + pick(s, lowerSeed, "other")
			a.SecurityGroupId = pulumi.String(v)
			return v
		}},
		{input: "clusterArn", set: func(a *A, s bool) string {
			v := "example-cluster-" + pick(s, testSeed, "other")
			a.ClusterArn = pulumi.String(v)
			return v
		}},
		{input: "executionRoleArn", set: func(a *A, s bool) string {
			v := "example-execution-role-" + pick(s, testSeed, "other")
			a.ExecutionRoleArn = pulumi.String(v)
			return v
		}},
		{input: "taskRoleArn", set: func(a *A, s bool) string {
			v := "example-task-role-" + pick(s, testSeed, "other")
			a.TaskRoleArn = pulumi.String(v)
			return v
		}},
		{input: "cloudMapNamespaceId", set: func(a *A, s bool) string {
			v := "ns-" + pick(s, lowerSeed, "other")
			a.CloudMapNamespaceId = pulumi.String(v)
			return v
		}},
		{input: "cloudMapNamespaceName", set: func(a *A, s bool) string {
			v := pick(s, splitSeed("."), "other") + ".internal.example"
			a.CloudMapNamespaceName = pulumi.String(v)
			return v
		}},
	}
}

func listenerSeedRows() []seedRow[envoyaws.WebhookListenerArgs] {
	type A = envoyaws.WebhookListenerArgs
	return []seedRow[A]{
		{input: "machineId", set: func(a *A, s bool) string {
			a.MachineID = pick(s, lowerSeed, "example-other-machine")
			return a.MachineID
		}},
		{input: "image", set: func(a *A, s bool) string {
			a.Image = "example.registry/" + pick(s, lowerSeed, "envoy-other") + ":placeholder"
			return a.Image
		}},
		{input: "region", set: func(a *A, s bool) string { a.Region = pick(s, testSeed, "example-region-2"); return a.Region }},
		{input: "webhooks[1]", set: func(a *A, s bool) string { a.Webhooks[1] = pick(s, testSeed, "slack"); return a.Webhooks[1] }},
		{input: "reviewerAppId", set: func(a *A, s bool) string {
			a.ReviewerAppId = pulumi.StringRef(pick(s, testSeed, "7654321"))
			return *a.ReviewerAppId
		}},
		{input: "cloudMapServiceName", set: func(a *A, s bool) string {
			a.CloudMapServiceName = pick(s, lowerSeed, "example-other-hooks")
			return a.CloudMapServiceName
		}},
		{input: "cloudMapServiceLogicalName", set: func(a *A, s bool) string {
			a.CloudMapServiceLogicalName = pick(s, testSeed, "example-other-cloudmap")
			return a.CloudMapServiceLogicalName
		}},
		{input: "family", set: func(a *A, s bool) string {
			a.Family = pick(s, splitSeed("-"), "example-other-family")
			return a.Family
		}},
		{input: "logGroupName", set: func(a *A, s bool) string {
			a.LogGroupName = "/ecs/" + pick(s, lowerSeed, "listener-other")
			return a.LogGroupName
		}},
		{input: "cpu", set: func(a *A, s bool) string { a.Cpu = pick(s, testSeed, "512"); return a.Cpu }},
		{input: "memory", set: func(a *A, s bool) string { a.Memory = pick(s, testSeed, "1024"); return a.Memory }},
		{input: "listenHost", set: func(a *A, s bool) string { a.ListenHost = pick(s, splitSeed("."), "::"); return a.ListenHost }},
		{input: "hostBridge", set: func(a *A, s bool) string { a.HostBridge = pick(s, lowerSeed, "127.0.0.2"); return a.HostBridge }},
		{input: "mentionTrigger", set: func(a *A, s bool) string {
			a.MentionTrigger = pulumi.StringRef("@" + pick(s, lowerSeed, "other"))
			return *a.MentionTrigger
		}},
		{input: "baseTags[project]", set: func(a *A, s bool) string {
			a.BaseTags["project"] = pick(s, testSeed, "example-other-project")
			return a.BaseTags["project"]
		}},
		{input: "a key of baseTags", set: func(a *A, s bool) string {
			key := pick(s, lowerSeed, "owner")
			a.BaseTags[key] = "example-owner"
			return key
		}},
		{input: "metricNamespace", set: func(a *A, s bool) string {
			a.MetricNamespace = "example/" + pick(s, testSeed, "Listener-other")
			return a.MetricNamespace
		}},
		// Inputs, refused once they resolve.
		{input: "natsUrls[0]", set: func(a *A, s bool) string {
			v := "nats://" + pick(s, splitSeed("."), "other") + ".internal.example:4222"
			a.NatsUrls = pulumi.ToStringArray([]string{v})
			return v
		}},
		{input: "apiTokenSecretArn", set: func(a *A, s bool) string {
			v := listenerSecretArn + pick(s, testSeed, "api-token-other")
			a.ApiTokenSecretArn = pulumi.String(v)
			return v
		}},
		{input: "apiTokenSecretVersionId", orders: true, set: func(a *A, s bool) string {
			v := pick(s, testSeed, "example-other-version")
			a.ApiTokenSecretVersionId = pulumi.String(v)
			return v
		}},
		{input: "seedSecretArn", set: func(a *A, s bool) string {
			v := listenerSecretArn + pick(s, testSeed, "nats-seed-other")
			a.SeedSecretArn = pulumi.String(v)
			return v
		}},
		{input: "githubWebhookSecretArn", set: func(a *A, s bool) string {
			v := listenerSecretArn + pick(s, lowerSeed, "github-other")
			a.GitHubWebhookSecretArn = pulumi.String(v)
			return v
		}},
		{input: "slackSigningSecretArn", set: func(a *A, s bool) string {
			v := listenerSecretArn + pick(s, testSeed, "slack-other")
			a.SlackSigningSecretArn = pulumi.String(v)
			return v
		}},
		{input: "clusterArn", set: func(a *A, s bool) string {
			v := "example-cluster-" + pick(s, testSeed, "other")
			a.ClusterArn = pulumi.String(v)
			return v
		}},
		{input: "executionRoleArn", set: func(a *A, s bool) string {
			v := "example-execution-role-" + pick(s, testSeed, "other")
			a.ExecutionRoleArn = pulumi.String(v)
			return v
		}},
		{input: "executionRoleName", set: func(a *A, s bool) string {
			v := "example-execution-" + pick(s, lowerSeed, "other")
			a.ExecutionRoleName = pulumi.String(v)
			return v
		}},
		{input: "taskRoleArn", set: func(a *A, s bool) string {
			v := "example-task-role-" + pick(s, testSeed, "other")
			a.TaskRoleArn = pulumi.String(v)
			return v
		}},
		{input: "subnetIds[0]", set: func(a *A, s bool) string {
			v := "subnet-" + pick(s, lowerSeed, "other")
			a.SubnetIds[0] = pulumi.String(v)
			return v
		}},
		{input: "securityGroupId", set: func(a *A, s bool) string {
			v := "sg-" + pick(s, lowerSeed, "other")
			a.SecurityGroupId = pulumi.String(v)
			return v
		}},
		{input: "targetGroupArn", set: func(a *A, s bool) string {
			v := "example-target-group-" + pick(s, testSeed, "other")
			a.TargetGroupArn = pulumi.String(v)
			return v
		}},
		{input: "cloudMapNamespaceId", set: func(a *A, s bool) string {
			v := "ns-" + pick(s, lowerSeed, "other")
			a.CloudMapNamespaceId = pulumi.String(v)
			return v
		}},
		{input: "cloudMapNamespaceName", set: func(a *A, s bool) string {
			v := pick(s, splitSeed("."), "other") + ".internal.example"
			a.CloudMapNamespaceName = pulumi.String(v)
			return v
		}},
		{input: "alarmTopicArn", set: func(a *A, s bool) string {
			v := alarmTopicArn + "-" + pick(s, testSeed, "other")
			a.AlarmTopicArn = pulumi.String(v)
			return v
		}},
		{input: "natsServiceArn", orders: true, set: func(a *A, s bool) string {
			v := "example-nats-service-" + pick(s, testSeed, "other")
			a.NatsServiceArn = pulumi.String(v)
			return v
		}},
	}
}

// A seed in any of NatsService's string inputs, or in its name, is refused naming the input by its
// path, before anything carries it: a plain input as the component resolves its inputs, an Input
// once it resolves, so no child is registered with it. Each row has a clean value of the same
// shape, which the component accepts and the run carries.
func TestNatsServiceRefusesASeedInAnyInput(t *testing.T) {
	requireSeedRefused(t, natsServiceType, "example-nats", natsSeedRows(), exampleArgs,
		func(t *testing.T, a *envoyaws.NatsServiceArgs) map[string]property.Value {
			return inputs(t, a, fromUpstream)
		})
	requireNameRefused(t, natsServiceType, func(t *testing.T) map[string]property.Value { return inputs(t, exampleArgs(), fromUpstream) })
}

// The same for WebhookListener.
func TestWebhookListenerRefusesASeedInAnyInput(t *testing.T) {
	requireSeedRefused(t, webhookListenerType, "example-listener", listenerSeedRows(), exampleListenerArgs,
		func(t *testing.T, a *envoyaws.WebhookListenerArgs) map[string]property.Value {
			return listenerInputsOf(t, a, fromUpstream)
		})
	requireNameRefused(t, webhookListenerType, func(t *testing.T) map[string]property.Value { return listenerInputs(t, fromUpstream) })
}

// Every input of both components' schemas that can carry a string has a row in the tables above,
// so a new input cannot go unchecked: each table's top-level names are exactly those fields'
// pulumi names.
func TestEveryStringInputHasASeedRow(t *testing.T) {
	stringFields := func(typ reflect.Type) []string {
		var names []string
		for i := range typ.NumField() {
			f := typ.Field(i)
			if k := f.Type.Kind(); k == reflect.Bool || k == reflect.Int || f.Type == reflect.TypeFor[*bool]() {
				continue
			}
			name, _, _ := strings.Cut(f.Tag.Get("pulumi"), ",")
			names = append(names, name)
		}
		slices.Sort(names)
		return names
	}
	topLevel := func(inputs []string) []string {
		var names []string
		for _, input := range inputs {
			input = strings.TrimPrefix(input, "a key of ")
			if i := strings.IndexAny(input, ".["); i >= 0 {
				input = input[:i]
			}
			names = append(names, input)
		}
		slices.Sort(names)
		return slices.Compact(names)
	}
	var nats, listener []string
	for _, row := range natsSeedRows() {
		nats = append(nats, row.input)
	}
	for _, row := range listenerSeedRows() {
		listener = append(listener, row.input)
	}
	require.Equal(t, stringFields(reflect.TypeFor[envoyaws.NatsServiceArgs]()), topLevel(nats))
	require.Equal(t, stringFields(reflect.TypeFor[envoyaws.WebhookListenerArgs]()), topLevel(listener))
}
