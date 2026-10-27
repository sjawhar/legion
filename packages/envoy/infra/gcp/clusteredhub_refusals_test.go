package gcp_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/gcp"
	"github.com/sjawhar/envoy/internal/testnats"
)

// clusteredRefusal is one invalid input: edit makes it of clusteredArgs, and the resolver refuses
// it as a core.InputError naming input, its message holding each of err.
type clusteredRefusal struct {
	name  string
	edit  func(*gcp.ClusteredNatsHubArgs)
	input string
	err   []string
}

// requireClusteredRefusals runs each row through the resolver, which must refuse it as a
// core.InputError naming the row's input, never quoting the tailnet auth key or the kubeconfig.
func requireClusteredRefusals(t *testing.T, rows []clusteredRefusal) {
	t.Helper()
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			args := clusteredArgs()
			row.edit(args)
			_, err := gcp.ResolveClusteredNatsHub(args)
			var input *core.InputError
			require.ErrorAs(t, err, &input)
			require.Equal(t, row.input, input.Input, "%v", err)
			for _, want := range row.err {
				require.ErrorContains(t, err, want)
			}
			require.NotContains(t, err.Error(), authKey)
			require.NotContains(t, err.Error(), kubeconfig)
		})
	}
}

// listener is args' tailnet listener, for the rows that edit it.
func listener(a *gcp.ClusteredNatsHubArgs) *gcp.ClusteredHubListenerArgs { return a.Tailnet.Listener }

// The resolver refuses every invalid plain input, naming it, with no engine; the constructor's own
// test shows a refusal stops it before anything registers.
func TestClusteredNatsHubResolverRefusesInvalidInputs(t *testing.T) {
	type A = gcp.ClusteredNatsHubArgs
	str := pulumi.StringRef
	// limit is INFRA-546's streamMaxBytes, for the sizing rows.
	limit := func(a *A) *A {
		a.StreamMaxBytes = pulumi.IntRef(infra546StreamMaxBytes)
		return a
	}
	requireClusteredRefusals(t, []clusteredRefusal{
		{"no kubeconfig", func(a *A) { a.Kubeconfig = nil }, "kubeconfig", []string{"kubeconfig is required"}},

		{"no name", func(a *A) { a.Name = "" }, "name", []string{`name ""`, "DNS-1123 label of at most 44 bytes"}},
		{"a 45-byte name", func(a *A) { a.Name = strings.Repeat("a", 45) }, "name", []string{"at most 44 bytes"}},
		{"a name with an uppercase letter", func(a *A) { a.Name = "Envoy-nats" }, "name", []string{`name "Envoy-nats"`}},
		{"a name ending in a hyphen", func(a *A) { a.Name = "envoy-" }, "name", []string{`name "envoy-"`}},

		{"no namespace", func(a *A) { a.Namespace = "" }, "namespace", []string{`namespace ""`}},
		{"a namespace with an underscore", func(a *A) { a.Namespace = "en_voy" }, "namespace", []string{`namespace "en_voy"`}},
		{"a 64-character namespace", func(a *A) { a.Namespace = strings.Repeat("a", 64) }, "namespace", []string{"DNS label"}},

		{"a service name starting with a digit", func(a *A) { a.ServiceName = "1nats" }, "serviceName", []string{`serviceName "1nats"`, "DNS-1035"}},
		{"a 64-character service name", func(a *A) { a.ServiceName = strings.Repeat("a", 64) }, "serviceName", []string{"DNS-1035"}},

		{"a NATS image without a digest", func(a *A) { a.NatsImage = "example.invalid/nats:2.15.1" }, "natsImage", []string{`natsImage "example.invalid/nats:2.15.1"`, "@sha256:"}},
		{"a reloader image without a digest", func(a *A) { a.ReloaderImage = "example.invalid/reloader:0.24.0" }, "reloaderImage", []string{"@sha256:"}},
		{"an exporter image without a digest", func(a *A) { a.ExporterImage = "example.invalid/exporter:0.20.2" }, "exporterImage", []string{"@sha256:"}},

		{"natsReplicas 0", func(a *A) { a.NatsReplicas = pulumi.IntRef(0) }, "natsReplicas", []string{"natsReplicas 0 must be from 1 to 3", "the hub runs 3 servers"}},
		{"natsReplicas 4", func(a *A) { a.NatsReplicas = pulumi.IntRef(4) }, "natsReplicas", []string{"natsReplicas 4 must be from 1 to 3"}},
		{"a negative natsReplicas", func(a *A) { a.NatsReplicas = pulumi.IntRef(-1) }, "natsReplicas", []string{"natsReplicas -1"}},
		{"streamMaxBytes 0", func(a *A) { a.StreamMaxBytes = pulumi.IntRef(0) }, "streamMaxBytes", []string{"streamMaxBytes 0 must be at least 1"}},

		{"maxFileStoreGb 0", func(a *A) { a.MaxFileStoreGb = pulumi.IntRef(0) }, "maxFileStoreGb", []string{"maxFileStoreGb 0 must be at least 1"}},
		{"maxFileStoreGb equal to the default disk", func(a *A) { a.MaxFileStoreGb = pulumi.IntRef(10) }, "maxFileStoreGb",
			[]string{"maxFileStoreGb 10 must be below diskSizeGb 10"}},
		{"maxFileStoreGb above diskSizeGb", func(a *A) { a.DiskSizeGb, a.MaxFileStoreGb = pulumi.IntRef(20), pulumi.IntRef(21) }, "maxFileStoreGb",
			[]string{"maxFileStoreGb 21 must be below diskSizeGb 20"}},
		{"diskSizeGb 1, at its default store", func(a *A) { a.DiskSizeGb = pulumi.IntRef(1) }, "diskSizeGb",
			[]string{"diskSizeGb 1 must be above maxFileStoreGb 1"}},
		{"diskSizeGb 0", func(a *A) { a.DiskSizeGb = pulumi.IntRef(0) }, "diskSizeGb", []string{"diskSizeGb 0 must be above maxFileStoreGb 1"}},
		{"diskSizeGb at an explicit store", func(a *A) { a.DiskSizeGb, a.MaxFileStoreGb = pulumi.IntRef(5), pulumi.IntRef(5) }, "maxFileStoreGb",
			[]string{"maxFileStoreGb 5 must be below diskSizeGb 5"}},
		// INFRA-546's limit needs S = 46.1 GiB of store and F ≈ 4.31 GiB (4409.4 MiB) of memory
		// (TestClusteredNatsHubSizingFollowsStreamMaxBytes, whose acceptance rows at 47, 4410Mi
		// and 4.5Gi these rows bound).
		{"maxFileStoreGb below S", func(a *A) { limit(a).MaxFileStoreGb = pulumi.IntRef(46) }, "maxFileStoreGb",
			[]string{"maxFileStoreGb 46 is below the 46.1 GiB that streamMaxBytes 44023414784 needs", "leave it unset for 47"}},
		{"diskSizeGb at the store streamMaxBytes needs", func(a *A) { limit(a).DiskSizeGb = pulumi.IntRef(47) }, "diskSizeGb",
			[]string{"diskSizeGb 47 must be above maxFileStoreGb 47, its default for streamMaxBytes 44023414784"}},
		{"natsMemory 4Gi below F", func(a *A) { limit(a).NatsMemory = str("4Gi") }, "natsMemory",
			[]string{`natsMemory "4Gi" is below the 4.31 GiB that streamMaxBytes 44023414784 needs`, "21170387 messages", "172.9 B each", "leave it unset for 5Gi"}},
		{"natsMemory a MiB below F", func(a *A) { limit(a).NatsMemory = str("4409Mi") }, "natsMemory",
			[]string{`natsMemory "4409Mi" is below the 4.31 GiB that streamMaxBytes 44023414784 needs`}},

		{"an empty storage class", func(a *A) { a.StorageClassName = str("") }, "storageClassName", []string{"storageClassName"}},
		{"a storage class with an underscore", func(a *A) { a.StorageClassName = str("premium_rwo") }, "storageClassName", []string{`storageClassName "premium_rwo"`}},

		{"an empty natsCpu", func(a *A) { a.NatsCpu = str("") }, "natsCpu", []string{"natsCpu is empty; leave it unset for 1"}},
		{"a natsCpu that is no quantity", func(a *A) { a.NatsCpu = str("fast") }, "natsCpu", []string{`natsCpu "fast" is not a Kubernetes quantity`}},
		{"a natsCpu exponent out of range", func(a *A) { a.NatsCpu = str("1e40") }, "natsCpu", []string{"decimal exponent"}},
		{"a natsCpu below Autopilot's floor", func(a *A) { a.NatsCpu = str("100m") }, "natsCpu",
			[]string{`natsCpu "100m" must be at least 400m`, "with the reloader's 50m and the exporter's 50m the hub pod", "less than 500m"}},
		{"a natsCpu above Autopilot's ceiling", func(a *A) { a.NatsCpu, a.NatsMemory = str("30"), str("64Gi") }, "natsCpu",
			[]string{`natsCpu "30" must be at most 29900m`}},
		{"an empty natsMemory", func(a *A) { a.NatsMemory = str("") }, "natsMemory", []string{"natsMemory is empty; leave it unset for 2Gi"}},
		{"a natsMemory that is no quantity", func(a *A) { a.NatsMemory = str("lots") }, "natsMemory", []string{`natsMemory "lots" is not a Kubernetes quantity`}},
		{"a natsMemory below Autopilot's floor", func(a *A) { a.NatsCpu, a.NatsMemory = str("400m"), str("256Mi") }, "natsMemory",
			[]string{`natsMemory "256Mi" must be at least 320Mi`}},
		{"a natsMemory above Autopilot's ceiling", func(a *A) { a.NatsCpu, a.NatsMemory = str("20"), str("110Gi") }, "natsMemory",
			[]string{`natsMemory "110Gi" must be at most 112448Mi`}},
		{"too little memory per CPU", func(a *A) { a.NatsCpu, a.NatsMemory = str("4"), str("2Gi") }, "natsMemory",
			[]string{`natsCpu "4" and natsMemory "2Gi"`, "between 1 and 6.5 GiB of memory per CPU"}},
		{"too much memory per CPU", func(a *A) { a.NatsCpu, a.NatsMemory = str("500m"), str("8Gi") }, "natsCpu",
			[]string{`natsCpu "500m" and natsMemory "8Gi"`, "between 1 and 6.5 GiB of memory per CPU"}},

		{"an empty priority class", func(a *A) { a.PriorityClassName = str("") }, "priorityClassName", []string{"priorityClassName is empty"}},
		{"a priority class with an uppercase letter", func(a *A) { a.PriorityClassName = str("Envoy") }, "priorityClassName", []string{"DNS subdomain"}},
		{"a system priority class", func(a *A) { a.PriorityClassName = str("system-cluster-critical") }, "priorityClassName", []string{"system- prefix"}},

		{"no client namespaces", func(a *A) { a.ClientNamespaces = nil }, "clientNamespaces", []string{"at least one namespace"}},
		{"a client namespace with an uppercase letter", func(a *A) { a.ClientNamespaces = []string{"envoy", "Dispatch"} }, "clientNamespaces",
			[]string{`clientNamespaces[1] "Dispatch" must be a DNS label`}},
		{"a repeated client namespace", func(a *A) { a.ClientNamespaces = []string{"envoy", "dispatch", "envoy"} }, "clientNamespaces",
			[]string{`clientNamespaces[2] "envoy" repeats clientNamespaces[0]`}},
		{"a monitor namespace with an underscore", func(a *A) { a.MonitorNamespaces = []string{"ops_team"} }, "monitorNamespaces",
			[]string{`monitorNamespaces[0] "ops_team"`}},
		{"a repeated monitor namespace", func(a *A) { a.MonitorNamespaces = []string{"ops", "ops"} }, "monitorNamespaces", []string{"repeats monitorNamespaces[0]"}},
		{"a metrics namespace with a dot", func(a *A) { a.MetricsNamespaces = []string{"gmp.system"} }, "metricsNamespaces", []string{`metricsNamespaces[0] "gmp.system"`}},
		{"a repeated metrics namespace", func(a *A) { a.MetricsNamespaces = []string{"gmp-system", "gmp-system"} }, "metricsNamespaces",
			[]string{"repeats metricsNamespaces[0]"}},

		{"tailnet without tailnetAuthKey", func(a *A) { a.TailnetAuthKey = nil }, "tailnetAuthKey", []string{"tailnetAuthKey is required with tailnet"}},
		{"tailnetAuthKey without tailnet", func(a *A) { a.Tailnet = nil }, "tailnetAuthKey", []string{"tailnetAuthKey is given without tailnet"}},
		{"natsReplicas without tailnet", func(a *A) { withoutTailnet(a).NatsReplicas = pulumi.IntRef(3) }, "natsReplicas",
			[]string{"natsReplicas is given without tailnet"}},
		{"a tailnet service name with an uppercase letter", func(a *A) { a.Tailnet.ServiceName = "Envoy-hub" }, "tailnet.serviceName",
			[]string{`tailnet.serviceName "Envoy-hub"`}},
		{"a tailnet DNS name outside ts.net", func(a *A) { a.Tailnet.TailnetDnsName = "example.com" }, "tailnet.tailnetDnsName",
			[]string{`tailnet.tailnetDnsName "example.com"`}},
		{"no tailnet tags", func(a *A) { a.Tailnet.Tags = nil }, "tailnet.tags", []string{"at least one tag"}},
		{"a tailnet tag without its prefix", func(a *A) { a.Tailnet.Tags = []string{"tag:envoy-hub-host", "envoy"} }, "tailnet.tags",
			[]string{`tailnet.tags[1] "envoy"`}},
		{"one tailnet replica", func(a *A) { a.Tailnet.Replicas = pulumi.IntRef(1) }, "tailnet.replicas", []string{"tailnet.replicas 1 must be at least 2"}},
		{"a Tailscale image without a digest", func(a *A) { a.Tailnet.TailscaleImage = "example.invalid/tailscale:latest" }, "tailnet.tailscaleImage",
			[]string{"@sha256:"}},
		{"tailnet without a listener", func(a *A) { a.Tailnet.Listener = nil }, "tailnet.listener", []string{"tailnet.listener is required with tailnet"}},
		{"a listener image without a digest", func(a *A) { listener(a).Image = "example.invalid/envoy:latest" }, "tailnet.listener.image",
			[]string{"@sha256:"}},
		{"an empty machine id", func(a *A) { listener(a).MachineID = str("") }, "tailnet.listener.machineId", []string{"tailnet.listener.machineId is empty"}},
		{"a machine id with a dot", func(a *A) { listener(a).MachineID = str("envoy.hub") }, "tailnet.listener.machineId", []string{"contains '.'"}},
		{"a machine id taking a user's durable", func(a *A) { listener(a).MachineID = str("example host a") }, "users[0].machineId",
			[]string{"users[0]: machineId gives the same durable name as the hub's own listener"}},
		{"no listener nkey", func(a *A) { listener(a).NKey = "" }, "tailnet.listener.nkey", []string{"tailnet.listener.nkey is required"}},
		{"a listener nkey that is no user key", func(a *A) { listener(a).NKey = "example-key" }, "tailnet.listener", []string{`listener "envoy-hub"`}},
		{"a seed secret name with an underscore", func(a *A) { listener(a).SeedSecretName = "seed_secret" }, "tailnet.listener.seedSecretName",
			[]string{`tailnet.listener.seedSecretName "seed_secret"`}},
		{"a seed secret key with a slash", func(a *A) { listener(a).SeedSecretKey = "a/b" }, "tailnet.listener.seedSecretKey",
			[]string{`tailnet.listener.seedSecretKey "a/b"`}},
		{"an allowed host with a port", func(a *A) { listener(a).AllowedHosts = []string{"envoy.example.com", "envoy.example.com:443"} },
			"tailnet.listener.allowedHosts", []string{"tailnet.listener.allowedHosts[1]"}},
		{"an empty listener cpu", func(a *A) { listener(a).Cpu = str("") }, "tailnet.listener.cpu", []string{"tailnet.listener.cpu is empty; leave it unset for 500m"}},
		{"a listener cpu that is no quantity", func(a *A) { listener(a).Cpu = str("half") }, "tailnet.listener.cpu", []string{"not a Kubernetes quantity"}},
		{"an empty listener memory", func(a *A) { listener(a).Memory = str("") }, "tailnet.listener.memory",
			[]string{"tailnet.listener.memory is empty; leave it unset for 1Gi"}},
		{"a listener memory that is no quantity", func(a *A) { listener(a).Memory = str("1 GB") }, "tailnet.listener.memory", []string{"not a Kubernetes quantity"}},
		{"a listener cpu that leaves the tailnet pod below Autopilot's floor", func(a *A) { listener(a).Cpu = str("100m") }, "tailnet.listener.cpu",
			[]string{"tailnet.listener.cpu \"100m\" must be at least 200m", "the tailnet pod would request less than 500m"}},
		{"a listener memory too small for the tailnet pod's CPU", func(a *A) { listener(a).Cpu = str("4"); listener(a).Memory = str("1Gi") }, "tailnet.listener.memory",
			[]string{"the tailnet pod must hold between 1 and 6.5 GiB of memory per CPU"}},

		{"no users and no tailnet", func(a *A) { withoutTailnet(a).Users = nil }, "users", []string{"at least one user is required"}},
		{"a user with no nkey", func(a *A) { a.Users[1].NKey = "" }, "users", []string{"users[1]: nkey is required"}},
		{"a user naming no grant", func(a *A) { a.Users[1].Permissions = nil }, "users", []string{"users[1]: name machineId"}},
		{"a user's machine id with a slash", func(a *A) { a.Users[0].MachineID = str("a/b") }, "users[0].machineId", []string{"users[0].machineId contains '/'"}},
		{"a session user with no nkey", func(a *A) { a.SessionUser.NKey = "" }, "sessionUser", []string{"sessionUser.nkey is required"}},
		{"a session user named as a user", func(a *A) { a.SessionUser.Name = "example-host-a" }, "sessionUser", []string{"name repeats users[0]"}},
	})
}

// clusteredSeedRow is one string input holding a seed: edit puts it there, and input is the path
// the refusal names.
type clusteredSeedRow struct {
	name, input string
	edit        func(a *gcp.ClusteredNatsHubArgs, seed string)
}

// clusteredSeedRows has a row for each string input the hub itself refuses a seed in, and one for
// each input whose seed refusal is core's (a user's and the session user's name and nkey, the
// listener's nkey), which the hub passes on.
func clusteredSeedRows() []clusteredSeedRow {
	type A = gcp.ClusteredNatsHubArgs
	str := pulumi.StringRef
	digest := "@sha256:" + strings.Repeat("0", 64)
	return []clusteredSeedRow{
		{"namespace", "namespace", func(a *A, s string) { a.Namespace = s }},
		{"name", "name", func(a *A, s string) { a.Name = strings.ToLower(s) }},
		{"serviceName", "serviceName", func(a *A, s string) { a.ServiceName = "nats-" + s }},
		{"natsImage", "natsImage", func(a *A, s string) { a.NatsImage = "example.invalid/" + s + digest }},
		{"reloaderImage", "reloaderImage", func(a *A, s string) { a.ReloaderImage = "example.invalid/" + strings.ToLower(s) + digest }},
		{"exporterImage", "exporterImage", func(a *A, s string) { a.ExporterImage = "example.invalid/" + splitSeed(s, ".") + digest }},
		{"storageClassName", "storageClassName", func(a *A, s string) { a.StorageClassName = str(strings.ToLower(s)) }},
		{"natsCpu", "natsCpu", func(a *A, s string) { a.NatsCpu = str(s) }},
		{"natsMemory", "natsMemory", func(a *A, s string) { a.NatsMemory = str(s + "Gi") }},
		{"priorityClassName", "priorityClassName", func(a *A, s string) { a.PriorityClassName = str("envoy-" + strings.ToLower(s)) }},
		{"clientNamespaces", "clientNamespaces[1]", func(a *A, s string) { a.ClientNamespaces[1] = strings.ToLower(s) }},
		{"monitorNamespaces", "monitorNamespaces[0]", func(a *A, s string) { a.MonitorNamespaces = []string{splitSeed(s, "-")} }},
		{"metricsNamespaces", "metricsNamespaces[0]", func(a *A, s string) { a.MetricsNamespaces = []string{s} }},
		{"a user's machine id", "users[0].machineId", func(a *A, s string) { a.Users[0].MachineID = str("host-" + s) }},
		{"a user's Dispatch machine id", "users[2].dispatchMachineId", func(a *A, s string) { a.Users[2].DispatchMachineID = str(splitSeed(s, " ")) }},
		{"tailnet.serviceName", "tailnet.serviceName", func(a *A, s string) { a.Tailnet.ServiceName = strings.ToLower(s) }},
		{"tailnet.tailnetDnsName", "tailnet.tailnetDnsName", func(a *A, s string) { a.Tailnet.TailnetDnsName = strings.ToLower(s) + ".ts.net" }},
		{"tailnet.tags", "tailnet.tags[0]", func(a *A, s string) { a.Tailnet.Tags = []string{"tag:" + s} }},
		{"tailnet.tailscaleImage", "tailnet.tailscaleImage", func(a *A, s string) { a.Tailnet.TailscaleImage = "example.invalid/" + s + digest }},
		{"tailnet.listener.image", "tailnet.listener.image", func(a *A, s string) { listener(a).Image = "example.invalid/" + s + digest }},
		{"tailnet.listener.machineId", "tailnet.listener.machineId", func(a *A, s string) { listener(a).MachineID = str(s) }},
		{"tailnet.listener.seedSecretName", "tailnet.listener.seedSecretName", func(a *A, s string) { listener(a).SeedSecretName = strings.ToLower(s) }},
		{"tailnet.listener.seedSecretKey", "tailnet.listener.seedSecretKey", func(a *A, s string) { listener(a).SeedSecretKey = s }},
		{"tailnet.listener.allowedHosts", "tailnet.listener.allowedHosts[0]", func(a *A, s string) {
			listener(a).AllowedHosts = []string{strings.ToLower(s) + ".example.com"}
		}},
		{"tailnet.listener.cpu", "tailnet.listener.cpu", func(a *A, s string) { listener(a).Cpu = str(s) }},
		{"tailnet.listener.memory", "tailnet.listener.memory", func(a *A, s string) { listener(a).Memory = str(s) }},
		// core's refusals, passed on: a seed in a name names the user; a seed as an nkey is refused
		// as no public key, without quoting it.
		{"a user's name", "users", func(a *A, s string) { a.Users[1].Name = s }},
		{"a user's nkey", "users", func(a *A, s string) { a.Users[1].NKey = s }},
		{"the session user's name", "sessionUser", func(a *A, s string) { a.SessionUser.Name = strings.ToLower(s) }},
		{"the session user's nkey", "sessionUser", func(a *A, s string) { a.SessionUser.NKey = s }},
		{"tailnet.listener.nkey", "tailnet.listener", func(a *A, s string) { listener(a).NKey = s }},
	}
}

// A seed in any string input is refused as a core.InputError naming the input by its path, never
// its value, before any other refusal of that input could quote it: the hub's own check runs first
// over every plain string input it carries, and core refuses one in a user's name or nkey.
func TestClusteredNatsHubRefusesASeedInAnyPlainInput(t *testing.T) {
	seed, _ := testnats.User(t)
	for _, row := range clusteredSeedRows() {
		t.Run(row.name, func(t *testing.T) {
			args := clusteredArgs()
			row.edit(args, seed)
			_, err := gcp.ResolveClusteredNatsHub(args)
			var input *core.InputError
			require.ErrorAs(t, err, &input)
			require.Equal(t, row.input, input.Input, "%v", err)
			if !slices.Contains([]string{"users", "sessionUser", "tailnet.listener"}, row.input) {
				require.EqualError(t, err, row.input+" holds an nkey seed (not echoed)")
			}
			require.NotContains(t, strings.ToUpper(err.Error()), seed)
		})
	}
}

// Every string input of the schema, top-level or inside tailnet and its listener, has a seed row,
// in clusteredSeedRows or, for an Input read only once it resolves, clusteredResolvedSeedInputs,
// so a new input cannot go unchecked. A user's and the session user's fields are core's, so those
// inputs need a row each, not one per field.
func TestClusteredNatsHubHasASeedRowForEveryStringInput(t *testing.T) {
	var stringFields func(typ reflect.Type, prefix string) []string
	stringFields = func(typ reflect.Type, prefix string) []string {
		var names []string
		for i := range typ.NumField() {
			f := typ.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("pulumi"), ",")
			switch f.Type {
			case reflect.TypeFor[int](), reflect.TypeFor[*int](), reflect.TypeFor[bool](), reflect.TypeFor[*bool]():
				continue
			case reflect.TypeFor[*gcp.ClusteredHubTailnetArgs](), reflect.TypeFor[*gcp.ClusteredHubListenerArgs]():
				names = append(names, stringFields(f.Type.Elem(), prefix+name+".")...)
				continue
			}
			names = append(names, prefix+name)
		}
		return names
	}
	want := stringFields(reflect.TypeFor[gcp.ClusteredNatsHubArgs](), "")
	slices.Sort(want)

	got := slices.Clone(clusteredResolvedSeedInputs)
	for _, row := range clusteredSeedRows() {
		path := row.name
		if i := strings.IndexAny(row.input, ".["); i >= 0 && (strings.HasPrefix(row.input, "users") || strings.HasPrefix(row.input, "sessionUser")) {
			path = row.input[:i]
		} else if row.input == "users" || row.input == "sessionUser" {
			path = row.input
		}
		got = append(got, path)
	}
	slices.Sort(got)
	require.Equal(t, want, slices.Compact(got))
}
