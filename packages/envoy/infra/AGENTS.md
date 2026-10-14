# envoy-infra

The Go module `github.com/sjawhar/envoy/infra`, which builds `pulumi-resource-envoy-infra`: the
Pulumi plugin whose components deploy Envoy. What it deploys, each component's inputs and
refusals, how an operator installs it and how an existing deployment moves onto it are in
`packages/envoy/infra/README.md`. This file is what changing the module needs on top of that.

## Its own workspace

The module is not in the repository's root `go.work`. In workspace mode a build selects the highest
version of each dependency that any workspace module requires, so listing this module there would
move the versions the Envoy binaries and the Legion daemon build with to whatever Pulumi needs.
Instead:

- `packages/envoy/infra/go.work` (`use (. ..)`) binds this module and `packages/envoy`. A go
  command run inside `packages/envoy/infra` finds it first: `go env GOWORK` there names it.
- `go.mod` replaces `github.com/sjawhar/envoy` with `../`, so the plugin compiles the Envoy
  packages it uses (`internal/bus`'s remote-NATS rule) from this checkout.
- Both being workspace modules keeps `scripts/go-third-party-notices.sh`, which ignores every
  module `go list -m` names, from listing Envoy's own packages as third-party code.

Run every go command for this module from `packages/envoy/infra`. From `packages/envoy`, `./...`
stops at this directory, since it is a module of its own. Make a dependency change here, in this
module's `go.mod` and `go.sum`; `packages/envoy/go.mod` and the root `go.work` keep their
versions. Never add the module to the root `go.work`.

## Layout

| Path | What it holds |
| --- | --- |
| `cmd/pulumi-resource-envoy-infra/` | The plugin's `main`. Its version is `-ldflags "-X main.version=<X.Y.Z>"`, `0.0.0-dev` without. |
| `provider/` | `provider.New`: the schema's metadata and the components the plugin serves. |
| `core/` | Logic that touches no cloud: `ListenerEnv`, the listener's environment, and `ListenerConfig.CheckPlain`, the part of its checks a component runs before it registers. |
| `host/` | `host:Listener` (`listener.go`) and its watchdog's texts (`watchdog.go`, golden-tested in `testdata/`). |
| `internal/providers/` | `Require`, the providers check. |
| `internal/pulumitest/` | The live tests' harness. |

## Rules for a component

- **Token.** A component's token is `envoy-infra:<package>:<Type>`, which pulumi-go-provider derives
  from its Go package and type. Its constructor registers with the same token as a constant, so it
  also runs outside the plugin, as the unit tests run it under `pulumi.WithMocks`, where no call
  carries one (`host/listener.go`, `listenerType`). `provider.New` serves it through
  `infer.ComponentF`.
- **Providers.** Before it registers anything, a component calls
  `providers.Require(ctx.Context(), <token>, <package>, opts...)` for each package it creates
  resources with, so a missing provider fails naming the component and the package, and no
  resource falls back to a default provider of the plugin's own SDK version. Its unit test runs it
  without each provider and asserts nothing registered.
- **Refusals.** Every refusal names the input. A component refuses every input it holds as a plain
  value before it registers anything, and its unit test asserts nothing but the test's providers
  registered. `NewListener` refuses an explicit `port` 0, then calls
  `core.ListenerConfig.CheckPlain` (a blank `machineId`, a `port` outside 1-65535, a `hostBridge`
  with surrounding whitespace), then the watchdog's three refusals and a missing provider
  (`host/listener.go:90-124`). Only `natsUrls`, which may be another resource's output, is checked
  later: `core.ListenerEnv` runs `CheckPlain` again and then checks each URL, inside the `ApplyT`
  over `natsUrls` that computes the container's environment (`:147-151`). A bad URL therefore fails
  the preview or update when the container's `envs` resolve, after the component and its image have
  registered, not the constructor.
- **Plain or Input.** A field of a plain Go type (`string`, `*int`, a pointer to a struct) takes
  only a value the consumer's program knows when it runs: Construct refuses another resource's
  output there, naming the input (Pulumi SDK v3.265.0 `go/pulumi/provider.go:395`, `:764-766`). A
  field a consumer takes from another resource or stack is a `pulumi.*Input`, as `natsUrls` and
  `seed` are, and gets a Construct test that sends it as another resource's output and as unknown
  under preview (`host/construct_test.go`), a path `pulumi.WithMocks` never takes.
- **Secrets.** Every input holding a secret carries `provider:"secret"`.
- **Object types.** An object input type has an `Annotate` that sets its token
  (`a.SetToken("host", "Watchdog")`): generated SDKs append `Args` to an input type's name, so the
  Go name would reach consumers as `WatchdogArgsArgs`.
- **Outputs.** Set a component's output fields after `RegisterComponentResource`, which replaces
  them.
- **Release contract.** `provider.go`'s `downloadURL` names the release the release job publishes:
  tag `envoy-infra-v<version>`, asset `pulumi-resource-envoy-infra-v<version>-<os>-<arch>.tar.gz`
  with the binary at the archive root. Changing one side breaks every consumer's install. A commit
  releases the plugin only when it touches a path `release.yaml` names for it, in its push paths,
  its `changes` job's `envoy_infra` filter and the bump paths after `--`, so a `packages/envoy`
  package the plugin starts compiling in, directly or through another import, goes into all three.
  `.github/scripts/check-envoy-infra-release-paths.sh` derives that set from `go list -deps` for
  every target and fails naming each path and list that misses it.
- **Byte-exact texts.** The watchdog's create and delete texts reproduce an existing deployment's
  byte for byte, since a `remote:Command` compares them as strings and any change reruns create on
  every host. Change them only with the goldens.

## Tests

The unit tests need neither the Pulumi CLI nor Docker. They run components under
`pulumi.WithMocks` (`host/listener_test.go`, `internal/providers/`) and the plugin in process
through pulumi-go-provider's `integration.NewServer` (`provider/provider_test.go`,
`host/construct_test.go`). The `envoy-infra-go` job in `.github/workflows/envoy-and-contracts.yaml`
runs, from `packages/envoy/infra`:

```sh
test -z "$(gofmt -l .)"
../../../.github/scripts/check-envoy-infra-release-paths.sh
go vet -tags pulumilive,natslive ./...
go test ./...
```

The release path check runs from any directory. Its own tests,
`.github/scripts/check-envoy-infra-release-paths.test.sh`, run in the `test` job of
`.github/workflows/pr-and-main.yaml`; run them after changing the check.

The live tests (`host/live_test.go`, build tag `pulumilive`) apply Pulumi YAML programs with the
`pulumi` CLI against this machine's Docker: a listener reaches `"status":"healthy"` on the explicit
docker provider and is gone after destroy; a listener that adopts a root-level container and image
previews with both `same`; a listener with a seed connects to an nkey-authorized NATS and one
without never does; and a Go program builds against the SDK `pulumi package add` generates. They
take:

- the `pulumi` CLI on `PATH`. CI pins v3.268.0 in `.github/actions/install-pulumi`, which the
  `envoy-smoke` job and release.yaml's `envoy_infra` job both install it through;
- `ENVOY_SMOKE_IMAGE`, a listener image built beforehand. A live test without it fails at once
  and never builds one itself;
- `ENVOY_INFRA_DOCKER_PLUGIN`, the path of the docker provider plugin v4.11.0 tarball for this
  machine's platform, whose SHA-256 the caller has checked. The test binary installs it into a
  `PULUMI_HOME` it makes for itself in a temporary directory and removes when its tests end, so the
  CLI downloads no plugin and the caller's own Pulumi home is untouched.

Each stack runs on a `file://` backend of its own in a temporary directory. Each listener gets a
container name of its own (`envoy-listener-live-<random>`) and a free port, and reaches only a
loopback NATS from `testnats`, never a tailnet name. The test binary builds the plugin once, on the
first test that asks for it, and every test runs that build. A package whose tests use the harness
calls `os.Exit(pulumitest.Main(m))` from its `TestMain`, which removes that `PULUMI_HOME` and that
build.

To run them, from the repository root:

```sh
curl -fsSL -o /tmp/pulumi-v3.268.0-linux-x64.tar.gz https://get.pulumi.com/releases/sdk/pulumi-v3.268.0-linux-x64.tar.gz
echo '903cafeaa9f6327e4bf53a8b7b99ae660f26198acfa804a2f982bf5de3f1de0b  /tmp/pulumi-v3.268.0-linux-x64.tar.gz' | sha256sum -c -
tar -xzf /tmp/pulumi-v3.268.0-linux-x64.tar.gz -C /tmp
docker buildx build --load -t legion-envoy-smoke:local -f packages/envoy/docker/Dockerfile .
curl -fsSL -o /tmp/pulumi-resource-docker-v4.11.0-linux-amd64.tar.gz https://github.com/pulumi/pulumi-docker/releases/download/v4.11.0/pulumi-resource-docker-v4.11.0-linux-amd64.tar.gz
echo '4dbcd9defa0fb03999cdfeab50bd461451b7c65e8566927994f63ee956055ffe  /tmp/pulumi-resource-docker-v4.11.0-linux-amd64.tar.gz' | sha256sum -c -
cd packages/envoy/infra && PATH=/tmp/pulumi:$PATH \
  ENVOY_SMOKE_IMAGE=legion-envoy-smoke:local \
  ENVOY_INFRA_DOCKER_PLUGIN=/tmp/pulumi-resource-docker-v4.11.0-linux-amd64.tar.gz \
  go test -count=1 -tags pulumilive -v -timeout 15m ./host/
```

`-count=1` because go's test cache cannot see the image behind a fixed tag, and would replay a
cached pass against an image that has since changed. In CI the `envoy-smoke` job runs them, each
input a step of its own (the CLI through `.github/actions/install-pulumi`), and then checks that
`pulumi package get-schema` reports the version the plugin was built with.
