// Package host holds the components that run Envoy on a Docker host.
package host

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/pulumi/pulumi-command/sdk/go/command/remote"
	"github.com/pulumi/pulumi-docker/sdk/v5/go/docker"
	"github.com/pulumi/pulumi-go-provider/infer"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/infra/internal/providers"
)

// listenerType is the component's token. pulumi-go-provider derives the same one from the
// Listener type's package and name, and serves it under that token; NewListener registers with
// the constant so it also runs outside the plugin, where no call carries a token.
const listenerType = "envoy-infra:host:Listener"

const (
	defaultContainerName = "envoy-listener"
	// waitTimeout is how long, in seconds, the docker provider waits for a new container's
	// health check to pass.
	waitTimeout = 90
)

// ListenerArgs are the inputs of an envoy-infra:host:Listener.
type ListenerArgs struct {
	MachineID     string                  `pulumi:"machineId"`
	NatsUrls      pulumi.StringArrayInput `pulumi:"natsUrls"`               // may come from another stack's output
	Image         string                  `pulumi:"image"`                  // e.g. ghcr.io/sjawhar/legion/envoy:<tag>
	ContainerName *string                 `pulumi:"containerName,optional"` // default "envoy-listener"
	Port          *int                    `pulumi:"port,optional"`          // default core.DefaultListenerPort, 9020
	HostBridge    *string                 `pulumi:"hostBridge,optional"`
	Seed          pulumi.StringInput      `pulumi:"seed,optional" provider:"secret"`
	Watchdog      *WatchdogArgs           `pulumi:"watchdog,optional"`
	Adopt         *AdoptArgs              `pulumi:"adopt,optional"`
	// default true; false for a listener whose NATS is not reachable from where the stack is
	// applied (the container is created and Docker restarts it until it connects)
	WaitForHealthy *bool `pulumi:"waitForHealthy,optional"`
}

// WatchdogArgs deploy the systemd watchdog that recreates a removed envoy-listener container.
type WatchdogArgs struct {
	Host string `pulumi:"host"` // the SSH address only; pulumi-command logs in as root (v1.2.1 remote/connection.go:57); "@" and "://" are refused
}

// Annotate names the type host:Watchdog in the schema: generated SDKs append Args to an object
// type's name for its input type, so the Go name would reach consumers as WatchdogArgsArgs.
func (w *WatchdogArgs) Annotate(a infer.Annotator) { a.SetToken("host", "Watchdog") }

// AdoptArgs name root-level resources this listener now owns; each child gets
// pulumi.Alias{Name: <name>, NoParent: true}.
type AdoptArgs struct {
	Container *string `pulumi:"container,optional"`
	Image     *string `pulumi:"image,optional"`
	Watchdog  *string `pulumi:"watchdog,optional"`
}

// Annotate names the type host:Adopt in the schema, for the reason WatchdogArgs's Annotate gives.
func (d *AdoptArgs) Annotate(a infer.Annotator) { a.SetToken("host", "Adopt") }

// Listener is one envoy-listener container on a Docker host, on the host's network.
type Listener struct {
	pulumi.ResourceState
	ContainerName pulumi.StringOutput `pulumi:"containerName"`
	Port          pulumi.IntOutput    `pulumi:"port"`
}

// listenerPlan is what a listener's inputs resolve to before anything registers.
type listenerPlan struct {
	containerName string
	// plain is every field of the listener's environment but NATSURLs, which may be another
	// resource's output and is known only when the container's envs resolve.
	plain    core.ListenerConfig
	adopt    AdoptArgs
	packages []string // the provider packages the listener creates resources with
}

// resolveListener gives each optional input its default and refuses every input it can check from
// the inputs alone: all of them but natsUrls and seed, which may be another resource's output. It
// registers nothing and needs no engine, so NewListener calls it first, and a refusal leaves
// nothing behind.
func resolveListener(name string, args *ListenerArgs) (listenerPlan, error) {
	plan := listenerPlan{
		containerName: defaultContainerName,
		plain:         core.ListenerConfig{MachineID: args.MachineID, Port: core.DefaultListenerPort},
		packages:      []string{"docker"},
	}
	if args.ContainerName != nil {
		plan.containerName = *args.ContainerName
	}
	if args.Port != nil {
		plan.plain.Port = *args.Port
	}
	if args.HostBridge != nil {
		plan.plain.HostBridge = *args.HostBridge
	}
	if args.Adopt != nil {
		plan.adopt = *args.Adopt
	}
	if err := plan.plain.CheckPlain(); err != nil {
		return listenerPlan{}, fmt.Errorf("%s %s: %w", listenerType, name, err)
	}
	if args.Watchdog == nil {
		// NewListener reads adopt.watchdog only with a watchdog to adopt it into, so without one
		// the old root-level command would be deleted, its delete text run as root on the host.
		if plan.adopt.Watchdog != nil {
			return listenerPlan{}, fmt.Errorf("%s %s: adopt.watchdog names %q, but watchdog is not set, so nothing takes it over",
				listenerType, name, *plan.adopt.Watchdog)
		}
		return plan, nil
	}
	// The watchdog's files and units are fixed per host and name the container envoy-listener.
	if plan.containerName != defaultContainerName {
		return listenerPlan{}, fmt.Errorf("%s %s: watchdog supervises only a container named %q, not containerName %q",
			listenerType, name, defaultContainerName, plan.containerName)
	}
	// It recreates a removed container from an env file that holds no secret, so the recreated
	// listener would come back without its seed.
	if args.Seed != nil {
		return listenerPlan{}, fmt.Errorf("%s %s: watchdog and seed cannot both be set: the watchdog recreates "+
			"a removed container from an env file that holds no secret", listenerType, name)
	}
	if strings.Contains(args.Watchdog.Host, "@") || strings.Contains(args.Watchdog.Host, "://") {
		return listenerPlan{}, fmt.Errorf("%s %s: watchdog.host must be the bare SSH address, with no user or "+
			"scheme (pulumi-command logs in as root): %q", listenerType, name, args.Watchdog.Host)
	}
	// The create script echoes the name inside double quotes, where the shell expands $, a
	// backtick and \, and where a line break would end the command.
	if strings.ContainsAny(name, "\"$`\\\n") {
		return listenerPlan{}, fmt.Errorf("%s %q: with a watchdog the name may not contain \", $, a backtick, \\ "+
			"or a newline, since the watchdog's script echoes it in double quotes", listenerType, name)
	}
	plan.packages = append(plan.packages, "command")
	return plan, nil
}

// NewListener registers a listener: the image, the container, and, with watchdog, the systemd
// watchdog that supervises it. It resolves its inputs, then requires a provider of each package it
// creates resources with, and only then registers anything.
func NewListener(ctx *pulumi.Context, name string, args *ListenerArgs, opts ...pulumi.ResourceOption) (*Listener, error) {
	plan, err := resolveListener(name, args)
	if err != nil {
		return nil, err
	}
	for _, pkg := range plan.packages {
		if err := providers.Require(ctx.Context(), listenerType, pkg, opts...); err != nil {
			return nil, err
		}
	}
	port := plan.plain.Port

	comp := &Listener{}
	if err := ctx.RegisterComponentResource(listenerType, name, comp, opts...); err != nil {
		return nil, err
	}
	// Registering replaces every output field with one the engine's answer resolves, so the
	// outputs are set after it.
	comp.ContainerName = pulumi.String(plan.containerName).ToStringOutput()
	comp.Port = pulumi.Int(port).ToIntOutput()

	image, err := docker.NewRemoteImage(ctx, name+"-image", &docker.RemoteImageArgs{
		Name:        pulumi.String(args.Image),
		KeepLocally: pulumi.Bool(true),
	}, childOptions(comp, plan.adopt.Image)...)
	if err != nil {
		return nil, err
	}

	listenerEnv := args.NatsUrls.ToStringArrayOutput().ApplyT(func(urls []string) ([]string, error) {
		config := plan.plain
		config.NATSURLs = urls
		return core.ListenerEnv(config)
	}).(pulumi.StringArrayOutput)
	envs := listenerEnv
	if args.Seed != nil {
		envs = pulumi.ToSecret(pulumi.All(listenerEnv, args.Seed).ApplyT(func(v []any) ([]string, error) {
			seed := v[1].(string)
			if strings.TrimSpace(seed) == "" {
				return nil, errors.New("seed is blank")
			}
			return append(slices.Clone(v[0].([]string)), core.EnvNATSSeed+"="+seed), nil
		})).(pulumi.StringArrayOutput)
	}

	containerArgs := &docker.ContainerArgs{
		Name:                pulumi.String(plan.containerName),
		Image:               image.ImageId,
		Command:             pulumi.StringArray{pulumi.String("/usr/local/bin/envoy-listener")},
		Restart:             pulumi.String("unless-stopped"),
		StopTimeout:         pulumi.Int(30),
		DestroyGraceSeconds: pulumi.Int(30),
		NetworkMode:         pulumi.String("host"),
		LogOpts:             pulumi.StringMap{"max-file": pulumi.String("5"), "max-size": pulumi.String("20m")},
		Envs:                envs,
		Healthcheck: docker.ContainerHealthcheckArgs{
			Tests: pulumi.StringArray{
				pulumi.String("CMD"), pulumi.String("curl"), pulumi.String("-f"),
				pulumi.String(fmt.Sprintf("http://127.0.0.1:%d/healthz", port)),
			},
			Interval:    pulumi.String("10s"),
			Timeout:     pulumi.String("3s"),
			Retries:     pulumi.Int(3),
			StartPeriod: pulumi.String("30s"),
		},
	}
	if args.WaitForHealthy == nil || *args.WaitForHealthy {
		containerArgs.Wait = pulumi.Bool(true)
		containerArgs.WaitTimeout = pulumi.Int(waitTimeout)
	} else {
		containerArgs.Wait = pulumi.Bool(false)
	}
	container, err := docker.NewContainer(ctx, name+"-container", containerArgs,
		append(childOptions(comp, plan.adopt.Container), pulumi.DeleteBeforeReplace(true))...)
	if err != nil {
		return nil, err
	}

	if args.Watchdog != nil {
		create := listenerEnv.ApplyT(func(envs []string) (string, error) {
			return watchdogCreate(name, envs, args.Image, port)
		}).(pulumi.StringOutput)
		if _, err := remote.NewCommand(ctx, name+"-watchdog", &remote.CommandArgs{
			Connection: remote.ConnectionArgs{Host: pulumi.String(args.Watchdog.Host)},
			Create:     create,
			Delete:     pulumi.String(watchdogDelete),
		}, append(childOptions(comp, plan.adopt.Watchdog), pulumi.DependsOn([]pulumi.Resource{container}))...); err != nil {
			return nil, err
		}
	}
	return comp, nil
}

// childOptions parents a child to the listener and, when adopted names a root-level resource,
// aliases the child to it, so the child takes over that resource's state.
func childOptions(comp *Listener, adopted *string) []pulumi.ResourceOption {
	opts := []pulumi.ResourceOption{pulumi.Parent(comp)}
	if adopted != nil {
		opts = append(opts, pulumi.Aliases([]pulumi.Alias{{Name: pulumi.String(*adopted), NoParent: pulumi.Bool(true)}}))
	}
	return opts
}
