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
- **Resolve, then require, then register.** A component's constructor first resolves its inputs in a
  pure function that needs no engine (`host.resolveListener`): it gives each optional input its
  default, refuses every input it holds as a plain value, and lists the provider packages the
  component creates resources with. The constructor then calls
  `providers.Require(ctx.Context(), <token>, <package>, opts...)` for each of those packages, so a
  missing provider fails naming the component and the package and no resource falls back to a
  default provider of the plugin's own SDK version, and only then registers anything. The refusal
  table runs against the resolver directly; one test drives the constructor with a refused input
  and one per missing provider, each asserting nothing registered beyond the test's providers.
- **Refusals.** Every refusal names the input. `core.ListenerConfig.CheckPlain` holds the checks of
  the listener's environment that the resolver can make (`machineId`, `port`, `hostBridge`);
  `core.ListenerEnv` runs it again and then checks each of `natsUrls`, the one field that may be
  another resource's output, inside the `ApplyT` that computes the container's environment. A bad
  URL, like a blank `seed` in its own `ApplyT`, therefore fails the preview or update when the
  container's `envs` resolve, after the component and its image have registered, not the
  constructor. The watchdog's create script runs as root on the host (`host.watchdogCreate`), so
  no value of the environment, which it writes into a heredoc, may hold a control character, and
  with a watchdog the listener's name, which it echoes in double quotes, may hold no `"`, `$`,
  backtick, `\` or line break.
- **Plain or Input.** A field of a plain Go type (`string`, `*int`, a pointer to a struct) takes
  only a value the consumer's program knows when it runs: Construct refuses another resource's
  output there, naming the input (Pulumi SDK v3.265.0 `go/pulumi/provider.go`,
  `constructInputsCopyTo` and `copyInputTo`). A field a consumer takes from another resource or
  stack is a `pulumi.*Input`, as `natsUrls` and `seed` are, and gets a Construct test that sends it
  as another resource's output and as unknown under preview (`host/construct_test.go`), a path
  `pulumi.WithMocks` never takes.
- **Secrets.** Every input holding a secret carries `provider:"secret"`.
- **Object types.** An object input type has an `Annotate` that sets its token
  (`a.SetToken("host", "Watchdog")`): generated SDKs append `Args` to an input type's name, so the
  Go name would reach consumers as `WatchdogArgsArgs`.
- **Outputs.** Set a component's output fields after `RegisterComponentResource`, which replaces
  them.
- **Release contract.** `provider.go`'s `downloadURL` names the release
  `.github/workflows/release-envoy-infra.yaml` publishes: tag `envoy-infra-v<version>`, asset
  `pulumi-resource-envoy-infra-v<version>-<os>-<arch>.tar.gz` with the binary at the archive root.
  Changing one side breaks every consumer's install. A version is cut only from what the plugin
  builds from: the workflow's version step lists, with `go list -deps` for each target, the
  directories of the workspace's modules the plugin compiles in, and adds those modules' `go.mod`
  and `go.sum`, the `go.work` files, the notices script and the target list. A commit to the
  module's README, AGENTS.md or live-test harness cuts none. The workflow's push paths take all of
  `packages/envoy/**`, so a package the plugin starts compiling in needs no list edited.
- **Release jobs.** The workflow's `build` job (`contents: read`, no secret) runs everything that
  runs the module's code or its dependencies: the version step's `go list`, vet, the unit tests,
  the build and the smoke test. It hands `publish` the archives as an artifact and the version as
  a job output, nothing else. `publish` holds the write scopes, the OIDC token and the deploy key,
  and runs only the pinned actions, `jq`, `sha256sum` and `release-push.sh`. It names the archives
  itself, from that version and its own checkout of the target list, one `archives+=` line per
  kind of archive; `release-push.sh handed` refuses a version that is not X.Y.Z and an artifact
  that is not exactly those archives, each a regular file, and prints the tag
  `envoy-infra-v<version>`; and `publish` writes `SHA256SUMS` over them. So `build` chooses the
  archives' bytes, and not their names, the tag or the sums. An archive the release adds gets its
  `archives+=` line in that step. The attestation runs after the tag step, so a run whose tag is
  refused leaves no attestation. A step that runs Go, the Pulumi CLI or the plugin goes in
  `build`, never in `publish`.
- **Byte-exact texts.** The watchdog's create and delete texts reproduce an existing deployment's
  byte for byte, since a `remote:Command` compares them as strings and any change reruns create on
  every host. Change them only with the goldens.

## Tests

The unit tests need neither the Pulumi CLI nor Docker. They run components under
`pulumi.WithMocks` (`host/listener_test.go`, `internal/providers/`), the refusals against
`host.resolveListener` directly, and the plugin in process through pulumi-go-provider's
`integration.NewServer` (`provider/provider_test.go`, `host/construct_test.go`). CI runs them in
the `envoy-infra-go` job of `.github/workflows/envoy-and-contracts.yaml`, with gofmt and vet under
the live tests' build tag; the release job runs vet and the unit tests again before it builds. The
required `lint` job of `.github/workflows/pr-and-main.yaml` compiles the module's commands for
every release target. Run those steps from those jobs after changing what they cover.

The live tests (`host/live_test.go`, build tag `pulumilive`) apply Pulumi YAML programs with the
`pulumi` CLI against this machine's Docker: a listener reaches `"status":"healthy"` on the explicit
docker provider and is gone after destroy; a listener that adopts a root-level container and image
previews with both `same`; a listener with a seed connects to an nkey-authorized NATS and one
without never does; and a Go program builds against the SDK `pulumi package add` generates. They
take:

- the `pulumi` CLI on `PATH`, the version `.github/actions/install-pulumi` installs;
- `ENVOY_SMOKE_IMAGE`, a listener image built beforehand. A live test without it fails at once
  and never builds one itself;
- `ENVOY_INFRA_DOCKER_PLUGIN`, the path of the docker provider plugin tarball of
  `pulumitest.DockerPluginVersion` for this machine's platform, whose SHA-256 the caller has
  checked. The test binary refuses a tarball of another version, installs it into a `PULUMI_HOME`
  it makes for itself in a temporary directory and removes when its tests end, so the CLI
  downloads no plugin and the caller's own Pulumi home is untouched.

Each stack runs on a `file://` backend of its own in a temporary directory. Each listener gets a
container name of its own (`envoy-listener-live-<random>`) and a free port, and reaches only a
loopback NATS from `testnats`, never a tailnet name. The test binary builds the plugin once, on the
first test that asks for it, and every test runs that build. A package whose tests use the harness
calls `os.Exit(pulumitest.Main(m))` from its `TestMain`, which removes that `PULUMI_HOME` and that
build.

To run them, install the CLI as `.github/actions/install-pulumi` does and download the docker
provider plugin as the `envoy-smoke` job's "Download the docker provider plugin the live tests pin"
step does, each checked against the SHA-256 pinned there; then, from the repository root:

```sh
docker buildx build --load -t legion-envoy-smoke:local -f packages/envoy/docker/Dockerfile .
cd packages/envoy/infra && ENVOY_SMOKE_IMAGE=legion-envoy-smoke:local \
  ENVOY_INFRA_DOCKER_PLUGIN=<the tarball> \
  go test -count=1 -tags pulumilive -v -timeout 15m ./host/
```

`-count=1` because go's test cache cannot see the image behind a fixed tag, and would replay a
cached pass against an image that has since changed. In CI the `envoy-smoke` job runs them, and
then checks that `pulumi package get-schema` reports the version the plugin was built with.
