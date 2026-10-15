package host_test

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/pulumi/pulumi-command/sdk/go/command"
	"github.com/pulumi/pulumi-docker/sdk/v5/go/docker"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/host"
)

const (
	containerType = "docker:index/container:Container"
	imageType     = "docker:index/remoteImage:RemoteImage"
	commandType   = "command:remote:Command"
	listenerType  = "envoy-infra:host:Listener"
	exampleImage  = "ghcr.io/sjawhar/legion/envoy:example"
)

// registration is one resource the mock engine was asked to register.
type registration struct {
	typ, name string
	inputs    resource.PropertyMap
	rpc       *pulumirpc.RegisterResourceRequest
}

// mocks records every registration. A RemoteImage's state gets an imageId, as the provider's
// does, so a container's image input can be checked against it.
type mocks struct {
	mu         sync.Mutex
	registered []registration
}

func (m *mocks) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.registered = append(m.registered, registration{
		typ: args.TypeToken, name: args.Name, inputs: args.Inputs, rpc: args.RegisterRPC,
	})
	state := args.Inputs.Copy()
	if args.TypeToken == imageType {
		state["imageId"] = resource.NewProperty("sha256:" + args.Name)
	}
	return args.Name + "-id", state, nil
}

func (m *mocks) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	return args.Args, nil
}

// one returns the single registration of type typ.
func (m *mocks) one(t *testing.T, typ string) registration {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	var found []registration
	for _, r := range m.registered {
		if r.typ == typ {
			found = append(found, r)
		}
	}
	require.Len(t, found, 1, "registrations of %s", typ)
	return found[0]
}

// types lists the type of every registration, in order.
func (m *mocks) types() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	types := make([]string, 0, len(m.registered))
	for _, r := range m.registered {
		types = append(types, r.typ)
	}
	return types
}

// run constructs a listener named example-host-a with args, passing it a provider of each package
// pkgs names, and returns what the mocks saw and the run's error. check, when not nil, runs on the
// constructed listener inside the program.
func run(t *testing.T, args *host.ListenerArgs, check func(*host.Listener), pkgs ...string) (*mocks, error) {
	t.Helper()
	m := &mocks{}
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		var provs []pulumi.ProviderResource
		for _, pkg := range pkgs {
			var (
				prov pulumi.ProviderResource
				err  error
			)
			switch pkg {
			case "docker":
				prov, err = docker.NewProvider(ctx, "docker-host", &docker.ProviderArgs{})
			case "command":
				prov, err = command.NewProvider(ctx, "command-host", &command.ProviderArgs{})
			default:
				t.Fatalf("no provider for package %q", pkg)
			}
			if err != nil {
				return err
			}
			provs = append(provs, prov)
		}
		l, err := host.NewListener(ctx, "example-host-a", args, pulumi.Providers(provs...))
		if err != nil {
			return err
		}
		if check != nil {
			check(l)
		}
		return nil
	}, pulumi.WithMocks("p", "s", m))
	return m, err
}

// exampleArgs are the existing Docker-host container's values, on placeholders.
func exampleArgs() *host.ListenerArgs {
	return &host.ListenerArgs{
		MachineID:  "example-host-a",
		NatsUrls:   pulumi.StringArray{pulumi.String("nats://hub.internal.example:4222")},
		Image:      exampleImage,
		HostBridge: pulumi.StringRef("127.0.0.1"),
	}
}

var exampleEnvs = []string{
	"PORT=9020",
	"ENVOY_MACHINE_ID=example-host-a",
	"NATS_URLS=nats://hub.internal.example:4222",
	"ENVOY_ALLOW_REMOTE_NATS=1",
	"ENVOY_HOST_BRIDGE=127.0.0.1",
}

// plain returns v's value with any secret wrapper removed, and whether there was one.
func plain(v resource.PropertyValue) (resource.PropertyValue, bool) {
	if v.IsSecret() {
		return v.SecretValue().Element, true
	}
	return v, false
}

// envsOf returns a container registration's envs and whether they are secret.
func envsOf(t *testing.T, r registration) ([]string, bool) {
	t.Helper()
	v, secret := plain(r.inputs["envs"])
	require.True(t, v.IsArray(), "envs is %s", v.TypeString())
	var envs []string
	for _, e := range v.ArrayValue() {
		envs = append(envs, e.StringValue())
	}
	return envs, secret
}

// withoutEnvs returns a registration's inputs as plain Go values, envs left out.
func withoutEnvs(r registration) map[string]any {
	in := r.inputs.Mappable()
	delete(in, "envs")
	return in
}

// exampleContainer is the container the defaults record for exampleArgs, envs left out.
var exampleContainer = map[string]any{
	"name":                "envoy-listener",
	"image":               "sha256:example-host-a-image",
	"command":             []any{"/usr/local/bin/envoy-listener"},
	"restart":             "unless-stopped",
	"stopTimeout":         float64(30),
	"destroyGraceSeconds": float64(30),
	"networkMode":         "host",
	"logOpts":             map[string]any{"max-file": "5", "max-size": "20m"},
	"healthcheck": map[string]any{
		"tests":       []any{"CMD", "curl", "-f", "http://127.0.0.1:9020/healthz"},
		"interval":    "10s",
		"timeout":     "3s",
		"retries":     float64(3),
		"startPeriod": "30s",
	},
	"wait":        true,
	"waitTimeout": float64(90),
}

func TestListenerDefaultsReproduceTheDockerHostContainer(t *testing.T) {
	// The listener's outputs are plain Outputs that RunErr does not wait for, so each capture
	// signals the WaitGroup the assertions below wait on.
	var captured sync.WaitGroup
	var containerName string
	var port int
	m, err := run(t, exampleArgs(), func(l *host.Listener) {
		captured.Add(2)
		l.ContainerName.ApplyT(func(v string) string { containerName = v; captured.Done(); return v })
		l.Port.ApplyT(func(v int) int { port = v; captured.Done(); return v })
	}, "docker")
	require.NoError(t, err)
	captured.Wait()

	comp := m.one(t, listenerType)
	require.Equal(t, "example-host-a", comp.name)

	image := m.one(t, imageType)
	require.Equal(t, "example-host-a-image", image.name)
	require.Equal(t, map[string]any{"name": exampleImage, "keepLocally": true}, image.inputs.Mappable())

	container := m.one(t, containerType)
	require.Equal(t, "example-host-a-container", container.name)
	require.Equal(t, exampleContainer, withoutEnvs(container))
	envs, secret := envsOf(t, container)
	require.ElementsMatch(t, exampleEnvs, envs)
	require.False(t, secret, "envs without a seed hold no secret")

	require.True(t, container.rpc.GetDeleteBeforeReplace(), "the container is deleted before it is replaced")
	for _, child := range []registration{image, container} {
		require.Equal(t, "urn:pulumi:s::p::"+listenerType+"::example-host-a", child.rpc.GetParent(),
			"%s's parent is the listener", child.name)
		require.Contains(t, child.rpc.GetProvider(), "::pulumi:providers:docker::docker-host::",
			"%s uses the docker provider the listener was given", child.name)
		require.Empty(t, child.rpc.GetAliases(), "%s is adopted from nothing", child.name)
	}
	require.NotContains(t, m.types(), commandType, "no watchdog without one asked for")

	require.Equal(t, "envoy-listener", containerName)
	require.Equal(t, 9020, port)
}

func TestListenerNatsUrlsFromAnOutputResolveIntoTheSameEnvs(t *testing.T) {
	args := exampleArgs()
	args.NatsUrls = pulumi.String("nats://hub.internal.example:4222").ToStringOutput().
		ApplyT(func(u string) []string { return []string{u} }).(pulumi.StringArrayOutput)
	m, err := run(t, args, nil, "docker")
	require.NoError(t, err)

	envs, _ := envsOf(t, m.one(t, containerType))
	require.ElementsMatch(t, exampleEnvs, envs)
}

func TestListenerSeedIsAppendedAndMakesTheEnvsSecret(t *testing.T) {
	args := exampleArgs()
	args.Seed = pulumi.String("SUEXAMPLESEED")
	m, err := run(t, args, nil, "docker")
	require.NoError(t, err)

	envs, secret := envsOf(t, m.one(t, containerType))
	require.ElementsMatch(t, append(slices.Clone(exampleEnvs), "NATS_NKEY_SEED=SUEXAMPLESEED"), envs)
	require.True(t, secret, "envs holding the seed are secret")
}

func TestListenerWaitForHealthy(t *testing.T) {
	t.Run("unset waits up to 90 s", func(t *testing.T) {
		m, err := run(t, exampleArgs(), nil, "docker")
		require.NoError(t, err)
		in := m.one(t, containerType).inputs
		require.Equal(t, resource.NewProperty(true), in["wait"])
		require.Equal(t, resource.NewProperty(float64(90)), in["waitTimeout"])
	})
	t.Run("false neither waits nor sets a timeout", func(t *testing.T) {
		args := exampleArgs()
		args.WaitForHealthy = pulumi.BoolRef(false)
		m, err := run(t, args, nil, "docker")
		require.NoError(t, err)
		in := m.one(t, containerType).inputs
		require.Equal(t, resource.NewProperty(false), in["wait"])
		require.NotContains(t, in, resource.PropertyKey("waitTimeout"))
	})
}

func TestListenerAdoptAliasesEachChildToItsRootLevelName(t *testing.T) {
	args := exampleArgs()
	args.Watchdog = &host.WatchdogArgs{Host: "example-host-a"}
	args.Adopt = &host.AdoptArgs{
		Container: pulumi.StringRef("listener-example-host-a"),
		Image:     pulumi.StringRef("envoy-image-example-host-a"),
		Watchdog:  pulumi.StringRef("watchdog-example-host-a"),
	}
	m, err := run(t, args, nil, "docker", "command")
	require.NoError(t, err)

	for typ, name := range map[string]string{
		containerType: "listener-example-host-a",
		imageType:     "envoy-image-example-host-a",
		commandType:   "watchdog-example-host-a",
	} {
		aliases := m.one(t, typ).rpc.GetAliases()
		require.Len(t, aliases, 1, "aliases of the %s", typ)
		spec := aliases[0].GetSpec()
		require.Equal(t, name, spec.GetName(), "alias of the %s", typ)
		require.True(t, spec.GetNoParent(), "the %s's alias has no parent", typ)
	}
}

func TestListenerRefusesMissingProvidersBeforeRegistering(t *testing.T) {
	t.Run("docker", func(t *testing.T) {
		m, err := run(t, exampleArgs(), nil)
		require.ErrorContains(t, err, `envoy-infra:host:Listener needs a "docker" provider in its providers option`)
		require.Empty(t, m.types())
	})
	t.Run("command, with a watchdog", func(t *testing.T) {
		args := exampleArgs()
		args.Watchdog = &host.WatchdogArgs{Host: "example-host-a"}
		m, err := run(t, args, nil, "docker")
		require.ErrorContains(t, err, `envoy-infra:host:Listener needs a "command" provider in its providers option`)
		require.Equal(t, []string{"pulumi:providers:docker"}, m.types())
	})
}

// resolveListener refuses every input it can check from the inputs alone, with no engine.
func TestListenerRefusesInvalidInputs(t *testing.T) {
	cases := []struct {
		name     string
		listener string // the listener's name; example-host-a when empty
		edit     func(*host.ListenerArgs)
		err      []string
	}{
		{
			name: "a watchdog for a container not named envoy-listener",
			edit: func(a *host.ListenerArgs) {
				a.Watchdog = &host.WatchdogArgs{Host: "example-host-a"}
				a.ContainerName = pulumi.StringRef("envoy-listener-b")
			},
			err: []string{"watchdog", `containerName "envoy-listener-b"`},
		},
		{
			name: "a watchdog with a seed",
			edit: func(a *host.ListenerArgs) {
				a.Watchdog = &host.WatchdogArgs{Host: "example-host-a"}
				a.Seed = pulumi.String("SUEXAMPLESEED")
			},
			err: []string{"watchdog and seed"},
		},
		{
			name: "a watchdog host with a scheme",
			edit: func(a *host.ListenerArgs) { a.Watchdog = &host.WatchdogArgs{Host: "ssh://example-host-a"} },
			err:  []string{"watchdog.host", `"ssh://example-host-a"`},
		},
		{
			name: "a watchdog host with a user",
			edit: func(a *host.ListenerArgs) { a.Watchdog = &host.WatchdogArgs{Host: "agent@example-host-a"} },
			err:  []string{"watchdog.host", `"agent@example-host-a"`},
		},
		{
			name: "adopt.watchdog without a watchdog",
			edit: func(a *host.ListenerArgs) {
				a.Adopt = &host.AdoptArgs{Watchdog: pulumi.StringRef("example-host-a-watchdog")}
			},
			err: []string{`adopt.watchdog names "example-host-a-watchdog", but watchdog is not set`},
		},
		{
			name: "port 0",
			edit: func(a *host.ListenerArgs) { a.Port = pulumi.IntRef(0) },
			err:  []string{"port 0 is outside 1-65535"},
		},
		{
			name: "a port above 65535",
			edit: func(a *host.ListenerArgs) { a.Port = pulumi.IntRef(65536) },
			err:  []string{"port 65536 is outside 1-65535"},
		},
		{
			name: "a blank machineId",
			edit: func(a *host.ListenerArgs) { a.MachineID = " " },
			err:  []string{"machineId is required"},
		},
		{
			name: "a host bridge with surrounding whitespace",
			edit: func(a *host.ListenerArgs) { a.HostBridge = pulumi.StringRef(" 127.0.0.1") },
			err:  []string{"hostBridge has leading or trailing whitespace", `" 127.0.0.1"`},
		},
		{
			name: "a machineId with a control character",
			edit: func(a *host.ListenerArgs) { a.MachineID = "a\nENVEOF\nid" },
			err:  []string{`machineId contains a control character: "a\nENVEOF\nid"`},
		},
		{
			name: "a host bridge with a control character inside",
			edit: func(a *host.ListenerArgs) { a.HostBridge = pulumi.StringRef("127.0.0.1\nENVEOF") },
			err:  []string{`hostBridge contains a control character: "127.0.0.1\nENVEOF"`},
		},
	}
	// The watchdog's create script echoes the listener's name inside double quotes, so a name
	// holding what the shell expands there, or a line break, is refused with a watchdog.
	for _, unsafe := range []string{`"`, "$", "`", `\`, "\n"} {
		listener := "example-host-a" + unsafe + "id"
		cases = append(cases, struct {
			name     string
			listener string
			edit     func(*host.ListenerArgs)
			err      []string
		}{
			name:     fmt.Sprintf("a watchdog on a listener named %q", listener),
			listener: listener,
			edit:     func(a *host.ListenerArgs) { a.Watchdog = &host.WatchdogArgs{Host: "example-host-a"} },
			err:      []string{fmt.Sprintf("%q", listener), "watchdog", `", $, a backtick, \ or a newline`},
		})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := exampleArgs()
			c.edit(args)
			listener := c.listener
			if listener == "" {
				listener = "example-host-a"
			}
			err := host.ResolveListener(listener, args)
			require.Error(t, err)
			for _, want := range c.err {
				require.ErrorContains(t, err, want)
			}
		})
	}
}

// NewListener resolves its inputs before it requires a provider or registers anything, so a
// refused input leaves only the program's own providers behind.
func TestListenerRefusesBeforeRegistering(t *testing.T) {
	args := exampleArgs()
	args.MachineID = "a\nENVEOF\nid"
	args.Watchdog = &host.WatchdogArgs{Host: "example-host-a"}
	m, err := run(t, args, nil, "docker", "command")
	require.ErrorContains(t, err, "machineId contains a control character")
	// The two provider registrations race each other, so only their set is fixed.
	require.ElementsMatch(t, []string{"pulumi:providers:docker", "pulumi:providers:command"}, m.types(),
		"nothing registered beyond the test's providers")
}

func TestListenerNatsUrlRefusedByListenerEnvFailsTheRun(t *testing.T) {
	args := exampleArgs()
	args.NatsUrls = pulumi.StringArray{pulumi.String("nats://a:1,nats://b:2")}
	_, err := run(t, args, nil, "docker")
	require.ErrorContains(t, err, "natsUrls[0] contains a comma")
}

// natsUrls may come from another stack, so a URL that would end the watchdog's env-file heredoc
// fails the run when the envs resolve, and no create script is rendered from it.
func TestListenerNatsUrlThatWouldEscapeTheWatchdogHeredocFailsTheRun(t *testing.T) {
	args := exampleArgs()
	args.NatsUrls = pulumi.StringArray{pulumi.String(
		"nats://hub.internal.example:4222\nENVEOF\ntouch /root/injected\ncat > /dev/null << 'ENVEOF'")}
	args.WaitForHealthy = pulumi.BoolRef(false)
	args.Watchdog = &host.WatchdogArgs{Host: "example-host-a"}
	m, err := run(t, args, nil, "docker", "command")
	require.ErrorContains(t, err, "natsUrls[0] contains a control character")
	for _, r := range m.registered {
		create, _ := plain(r.inputs["create"])
		require.False(t, create.IsString() && strings.Contains(create.StringValue(), "touch /root/injected"),
			"%s %s carries the URL's command", r.typ, r.name)
	}
}

// A listener's name is refused only with a watchdog, whose script echoes it.
func TestListenerNameIsFreeWithoutAWatchdog(t *testing.T) {
	require.NoError(t, host.ResolveListener("example-host-$a", exampleArgs()))
}

// A blank seed would start a listener that refuses its NATS_NKEY_SEED and restarts until the
// health wait gives up, naming no input, so it fails the run naming seed.
func TestListenerBlankSeedFailsTheRun(t *testing.T) {
	args := exampleArgs()
	args.Seed = pulumi.String(" ")
	_, err := run(t, args, nil, "docker")
	require.ErrorContains(t, err, "seed is blank")
}

// watchdogOf constructs a listener with a watchdog on example-host-a and returns its command's
// registration.
func watchdogOf(t *testing.T, args *host.ListenerArgs) (registration, *mocks) {
	t.Helper()
	args.Watchdog = &host.WatchdogArgs{Host: "example-host-a"}
	m, err := run(t, args, nil, "docker", "command")
	require.NoError(t, err)
	return m.one(t, commandType), m
}

func readGolden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)
	return string(b)
}

func TestListenerWatchdogTextsAreTheDockerHostProgramsByteForByte(t *testing.T) {
	cmd, m := watchdogOf(t, exampleArgs())

	require.Equal(t, "example-host-a-watchdog", cmd.name)
	require.Equal(t, readGolden(t, "watchdog-create.golden"), cmd.inputs["create"].StringValue())
	require.Equal(t, readGolden(t, "watchdog-delete.golden"), cmd.inputs["delete"].StringValue())

	conn, _ := plain(cmd.inputs["connection"])
	require.Equal(t, "example-host-a", conn.ObjectValue()["host"].StringValue())
	require.Contains(t, cmd.rpc.GetProvider(), "::pulumi:providers:command::command-host::")
	container := m.one(t, containerType)
	require.Contains(t, cmd.rpc.GetDependencies(),
		"urn:pulumi:s::p::"+listenerType+"$"+containerType+"::"+container.name,
		"the watchdog is deployed after the container")
}

func TestListenerWatchdogOnAnotherPortMovesOnlyItsTwoPortLines(t *testing.T) {
	args := exampleArgs()
	args.Port = pulumi.IntRef(9021)
	cmd, _ := watchdogOf(t, args)

	want := strings.Split(readGolden(t, "watchdog-create.golden"), "\n")
	got := strings.Split(cmd.inputs["create"].StringValue(), "\n")
	require.Len(t, got, len(want))
	var changed [][2]string
	for i := range want {
		if want[i] != got[i] {
			changed = append(changed, [2]string{want[i], got[i]})
		}
	}
	require.Equal(t, [][2]string{
		{"PORT=9020", "PORT=9021"},
		{`  "healthcheck_cmd": "curl -sf http://127.0.0.1:9020/healthz"`, `  "healthcheck_cmd": "curl -sf http://127.0.0.1:9021/healthz"`},
	}, changed)
	require.Equal(t, readGolden(t, "watchdog-delete.golden"), cmd.inputs["delete"].StringValue())
}
