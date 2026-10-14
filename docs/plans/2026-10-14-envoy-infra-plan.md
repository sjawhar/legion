# Envoy infrastructure components: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** a Go Pulumi plugin, `envoy-infra`, that deploys Envoy's NATS server and listeners, so a
new deployment needs configuration rather than infrastructure code of its own.

**Architecture:** a new Go module at `packages/envoy/infra` (module path
`github.com/sjawhar/envoy/infra`) builds `pulumi-resource-envoy-infra`, an executable plugin made
with `pulumi-go-provider`'s `infer` package, and in slice 3 an `envoy-infra` CLI. A `core` package
holds cloud-free logic (listener environment, NATS authorization, permission sets). Component
packages (`host`, `gcp`) hold Pulumi components whose tokens are `envoy-infra:<package>:<Type>`.
The plugin is released from `release.yaml` as tarballs under the tag `envoy-infra-v<version>`, and
the schema names an `https` download URL with `${VERSION}`.

**Tech stack:** Go 1.26.8, pulumi-go-provider v1.7.0 (requires Pulumi `sdk/v3` and `pkg/v3`
v3.265.0), pulumi-docker SDK v5.2.0, pulumi-command SDK v1.2.1, pulumi-gcp SDK v10.1.0 (slice 3;
v10.1.1 would raise `sdk/v3` to v3.268.0), nkeys v0.4.15 and nats.go v1.50.0, testcontainers-go
v0.41.0, Pulumi CLI v3.268.0 in CI.

**Spec:** `docs/plans/2026-10-14-envoy-infra-design.md` (approved by Sami, 2026-10-13 and
2026-10-14). The evidence for every pin and call is summarised under "Evidence" at the end.

## Global constraints

- Legion is public. No file, commit, PR title, body, branch or comment names the private
  repository the AWS footprint comes from, the company that runs it, a private host, a tailnet
  name, a machine name of a real deployment, or an account id. Placeholders follow the root
  `AGENTS.md` (`example-host-a`, `<name>.internal.example`). `.github/scripts/check-private-names.sh`
  passes at every head. Port logic, never provenance comments or incident narratives.
- Plain git in every clone (no `.jj` in them). Never jj.
- The Envoy binaries' module (`packages/envoy`) and the daemon's (`packages/daemon`) keep their
  dependency versions: `packages/envoy/go.mod`, `go.sum`, `packages/daemon/go.mod`, `go.sum`, the
  root `go.work` and `go.work.sum` change only where a task below says so.
- Every component creates its resources with providers the consumer passes in (`providers`
  resource option). A component missing one fails before it registers anything, naming the
  component and the package.
- Every component input holding a secret carries `provider:"secret"`.
- An existing deployment moves onto a component only when its `pulumi preview` creates, updates,
  replaces or deletes no cloud resource, the only creates being component resources, with stack
  outputs unchanged (Sami, 2026-10-13).
- No defensive guards around build invariants; no silent fallbacks. A missing or invalid input is
  an error naming the input.
- A live test proves a listener is up only by `/healthz` answering `"status":"healthy"`
  (`packages/envoy/cmd/listener/main.go:257-271`: it answers 200 `"starting"` before NATS). A
  negative control asserts "healthy" never arrives within its window and shows a positive sign of
  the failure (the authorization violation in the listener's log, or Docker `RestartCount` > 0).
- Live programs use only a loopback NATS URL from `testnats`; never `envoy-nats` or any tailnet
  name (on a machine with a tailnet, a bare name can resolve to a production server).
- Release tags are `envoy-infra-v<X.Y.Z>`; plugin assets are
  `pulumi-resource-envoy-infra-v<X.Y.Z>-<os>-<arch>.tar.gz` with the binary at the archive root;
  the schema's `pluginDownloadURL` is exactly
  `https://github.com/sjawhar/legion/releases/download/envoy-infra-v${VERSION}`.
- `gofmt` clean; tests beside the code; CI's exact commands (listed per task) pass locally before
  every push.

## Decisions carried from the design conversation

Sami, 2026-10-13 and 2026-10-14, recorded in the spec:

- Go, shipped as an executable plugin; nested module under `packages/envoy`.
- The preview rule above for every existing deployment that moves.
- No webhook ingress on the GCP hub in the first version; Dispatch is out of scope.
- One NATS user per host, shared by that host's listener and its sessions.
- Agents are trusted like an engineer who is not a super-admin; restricting them is not a goal.
- The AWS components (`aws:*`, spec "Order" 2) and the move of the existing AWS deployment onto
  them come after this plan, "eventually" (Sami, 2026-10-13); they get their own plan once the
  first GCP deployment is accepted.

Rejected, with the reason:

- TypeScript or Python components: every consumer would need that runtime.
- Pulumi's `github://` download scheme: it looks only for bare `v<version>` tags.
- `compute.PerInstanceConfig` for the hub's disk: its create path raises the group's target size.

## Rulings made while planning

- **R1. The module is not in the root `go.work`.** In workspace mode every build selects the
  highest version of each shared dependency across the workspace, so listing the module there
  would move the Envoy binaries' and the daemon's transitive dependencies to whatever Pulumi
  needs. The module carries its own `packages/envoy/infra/go.work` with `use (. ..)`, found first
  by any go command run inside it, and `replace github.com/sjawhar/envoy => ../` in its `go.mod`
  (the same pattern as `packages/daemon/go.mod:136`). Both modules being workspace modules there
  keeps `scripts/go-third-party-notices.sh` (which ignores the modules `go list -m` names, `:51`)
  from treating Envoy's own packages as third-party code. Task 1.1 corrects the spec's "added to
  `go.work`". Cost if wrong: one `go.work` edit.
- **R2. Release targets live in their own file**, `.github/envoy-infra-targets.json`.
  `.github/go-release-targets.json` is a patch path of both the `legion` and `legion-envoy`
  releases (`release.yaml:279-284`, `release-envoy-listener.yaml:375-381`), so a key added there
  would cut two releases that change nothing. Precedent: `.github/agent-secrets-client-targets.json`.
  Task 1.1 corrects the spec's release sentence.
- **R3. `packages/envoy/infra/**` leaves the Envoy image release.** `release-envoy-listener.yaml`
  triggers on `packages/envoy/**` (`:13`) and bumps from it (`:379`), so an infra commit would
  publish a listener image and cut a `legion-envoy` release with no listener change. Task 1.4 adds
  `!packages/envoy/infra/**` to its `paths` (the trigger check reads `!` as a negation,
  `check-image-trigger-paths.sh:190-208`) and `':(exclude)packages/envoy/infra'` to its bump paths
  (`release-bump.sh:53` passes them to `git log` unchanged). The `envoy-and-contracts.yaml` filters
  keep it, so `envoy-smoke` still builds the listener image the live tests need.
- **R4. `host:Listener` takes an `adopt` input and an optional seed.** A consumer could alias a
  remote component's children with a transform in its own language, but `adopt` saves every
  consumer from writing one; and the existing Docker-host container has no seed.
- **R5. The watchdog supervises one listener per host, named `envoy-listener`.** Its files and
  unit names are fixed per host (`/etc/envoy/*`, `envoy-watchdog.*`), so `watchdog` with another
  `containerName` is refused. Its text substitutes the port (unchanged at 9020) and the
  component's name (the existing program echoes `machine.name`, `watchdog.ts:155`).
- **R6. The NATS authorization model, the seed command and `gcp:NatsHub` ship together in slice
  3.** Their live proof needs a billed GCP project with Secret Manager; slice 1 needs neither.
- **R7. An authorization with no users and no `no_auth_user` is refused.** nats-server treats an
  empty user list as no authentication at all (v2.15.1 `server/auth.go:328-337`).
- **R8. Release paths include the Envoy packages the plugin compiles in.** The plugin renders
  grants from Envoy's exported subject names, so a change there must cut a plugin release; a CI
  check holds the list to `go list -deps`.
- **R9. `host:Listener` offers no listen host and no webhooks in this version.** No consumer here
  sets webhooks (the Docker-host deployment's machines all have `listener: {}`; the GCP hub has
  none in the first version, Sami 2026-10-13), and a non-loopback listener needs an API token
  (`cmd/listener/main.go:420-429`) that nothing here provides. Task 1.1 corrects the spec's list.
- **R10. A watchdog and a seed are refused together.** The watchdog recreates a removed container
  from its env file alone (`watchdog.ts`, the `missing)` branch), which holds no secret, so a
  recreated listener would come back without its seed.

## Slices

| Slice | PR | Outcome someone can try | Depends on |
| --- | --- | --- | --- |
| 1 | sjawhar/legion#1946 | `pulumi package add envoy-infra@<v> --server '<url>'` installs a released plugin, and `envoy-infra:host:Listener` runs a healthy listener on a Docker host | none |
| 3 | a new PR stacked on #1946 (gh-stack) | `envoy-infra seed generate` writes an nkey seed into Secret Manager and prints its public key; `envoy-infra:gcp:NatsHub` serves NATS on GCE over a tailnet | Task 1.1 on `envoy-infra` |

(Slices 2 and 4 are the consumers' changes, planned in their own repositories.)

---

## Slice 1: the module, `host:Listener`, the release

### Task 1.1: module skeleton, plugin entry, schema

**Files:**
- Create: `packages/envoy/infra/go.mod`, `go.sum`, `go.work`, `go.work.sum`
- Create: `packages/envoy/infra/cmd/pulumi-resource-envoy-infra/main.go`
- Create: `packages/envoy/infra/provider/provider.go`, `provider_test.go`
- Create: `packages/envoy/infra/internal/providers/require.go`, `require_test.go`
- Modify: `docs/plans/2026-10-14-envoy-infra-design.md` (R1, R2)
- Modify: `.github/workflows/envoy-and-contracts.yaml` (a new `envoy-infra-go` job)

**Interfaces:**
- Produces: `provider.New() (p.Provider, error)` registering every component;
  `providers.Require(ctx context.Context, component, pkg string, opts ...pulumi.ResourceOption) error`.

- [ ] **Step 1: module files.** `go.mod`:

```
module github.com/sjawhar/envoy/infra

go 1.26.8

require (
	github.com/pulumi/pulumi-go-provider v1.7.0
	github.com/pulumi/pulumi/sdk/v3 v3.265.0
	github.com/sjawhar/envoy v0.0.0
)

replace github.com/sjawhar/envoy => ../
```

`go.work`: `go 1.26.8` and `use (. ..)`. Run `cd packages/envoy/infra && go mod tidy`. Confirm R1:
`git diff --exit-code -- go.work go.work.sum packages/envoy/go.mod packages/envoy/go.sum packages/daemon/go.mod packages/daemon/go.sum`
prints nothing; `cd packages/envoy && go build ./... && go list ./... | grep -c infra` prints `0`;
`cd packages/daemon && go build ./...` passes; `cd packages/envoy/infra && go env GOWORK` names
`packages/envoy/infra/go.work`. If any of these fails, stop and put the command and its output in
your returned report; do not add the module to the root `go.work`.

- [ ] **Step 2: failing test for `Require`** (`internal/providers/require_test.go`), with
`pulumi.RunErr` and `pulumi.WithMocks`:

```go
func TestRequireFindsTheProviderForThePackage(t *testing.T) {
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		prov, err := docker.NewProvider(ctx, "docker-host", &docker.ProviderArgs{})
		if err != nil {
			return err
		}
		return providers.Require(ctx.Context(), "envoy-infra:host:Listener", "docker", pulumi.Providers(prov))
	}, pulumi.WithMocks("p", "s", mocks{}))
	require.NoError(t, err)
}

func TestRequireRefusesAMissingProviderNamingComponentAndPackage(t *testing.T) {
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		return providers.Require(ctx.Context(), "envoy-infra:host:Listener", "docker")
	}, pulumi.WithMocks("p", "s", mocks{}))
	require.ErrorContains(t, err, `envoy-infra:host:Listener needs a "docker" provider in its providers option`)
}

func TestRequireRefusesAProviderOfAnotherPackage(t *testing.T) {
	// a command provider passed where docker is required: same message as above
}
```

`Require` reads `pulumi.NewResourceOptions(opts...)`'s `Provider` and `Providers`, awaits each
provider's URN with `internals.UnsafeAwaitOutput`, and matches `urn.Type().Name()` (a provider's
type is `pulumi:providers:<pkg>`) against `pkg`. Run
`cd packages/envoy/infra && go test ./internal/providers/`, see it fail, implement, see it pass.

- [ ] **Step 3: failing schema test.** `provider/provider_test.go` serves `provider.New()` in
process with `integration.NewServer` (pulumi-go-provider `integration`), calls `GetSchema`, and
asserts `pluginDownloadURL == "https://github.com/sjawhar/legion/releases/download/envoy-infra-v${VERSION}"`
and `language.go.importBasePath == "github.com/sjawhar/envoy/infra/sdk/go/envoyinfra"`. (Task 1.3
adds an assertion that `resources` lists `envoy-infra:host:Listener` with `isComponent: true`.)

- [ ] **Step 4: implement.** `provider.New` is
`infer.NewProviderBuilder().WithNamespace("sjawhar").WithDisplayName("Envoy infrastructure").WithRepository("https://github.com/sjawhar/legion").WithLicense("Apache-2.0").WithPluginDownloadURL(downloadURL).WithGoImportPath("github.com/sjawhar/envoy/infra/sdk/go/envoyinfra").WithComponents(...).Build()`
(without `WithGoImportPath` the default is `<repository URL>/sdk/go/<pkg>`, which Go rejects as a
module path, pulumi-go-provider v1.7.0 `infer/provider_builder.go:74-80`). `main.go`:

```go
var version = "0.0.0-dev" // -ldflags "-X main.version=<X.Y.Z>" in the release job

func main() {
	prov, err := provider.New()
	if err != nil {
		fmt.Fprintf(os.Stderr, "envoy-infra: %v\n", err)
		os.Exit(1)
	}
	if err := prov.Run(context.Background(), "envoy-infra", version); err != nil {
		fmt.Fprintf(os.Stderr, "envoy-infra: %v\n", err)
		os.Exit(1)
	}
}
```

- [ ] **Step 5: notices.** From `packages/envoy/infra`, run
`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 ../../../scripts/go-third-party-notices.sh /tmp/<scratch>/notices ./cmd/pulumi-resource-envoy-infra`
and require exit 0 and no `github.com/sjawhar/envoy` entry in the output. If it lists Envoy's
packages as third-party, extend the script to also ignore modules replaced by a local directory
(`go list -m -f '{{if and .Replace (not .Replace.Version)}}--ignore={{.Path}}{{end}}' all`) with
its header comment updated, and report that this cuts patch releases of `legion` and
`legion-envoy` (the script is a patch path of both).

- [ ] **Step 6: CI job.** Add `envoy-infra-go` to `envoy-and-contracts.yaml`, gated on the
existing `envoy` filter, modelled on `envoy-go` (`:744-811`): SHA-pinned checkout and setup-go
(`go-version-file: packages/envoy/infra/go.mod`, `cache-dependency-path: packages/envoy/infra/go.sum`),
`TESTCONTAINERS_RYUK_DISABLED: "true"`, the NATS image pre-pull reading
`packages/envoy/internal/testnats/nats.go`'s `const Image` (Task 3.1 adds `const HubImage` to it),
then in `packages/envoy/infra`: `test -z "$(gofmt -l .)"`, `go vet -tags pulumilive,natslive ./...`,
`go test ./...`. The workflow
has no aggregate job (verified), so nothing else lists it.

- [ ] **Step 7: correct the spec** (R1, R2). Replace "nested under `packages/envoy` and added to
`go.work`, so Pulumi and its provider SDKs stay out of the module the Envoy binaries build from."
with "nested under `packages/envoy` with a `go.work` of its own, not the repository's, so Pulumi
and its provider SDKs stay out of the Envoy binaries' module and their dependency versions.";
and in "Release", replace "from `.github/go-release-targets.json`" with "from
`.github/envoy-infra-targets.json` (a file of its own, since the shared targets file is a release
input of the other Go binaries)"; and in `core`'s list, replace "the remote-NATS opt-in, port,
listen host, seed and webhooks" with "the remote-NATS opt-in, port, host bridge and seed" (R9).

- [ ] **Step 8: commit** `feat(envoy-infra): module skeleton and plugin entry`.

### Task 1.2: the listener's environment (`core`)

**Files:**
- Create: `packages/envoy/infra/core/listener_env.go`, `listener_env_test.go`
- Modify: `packages/envoy/internal/bus/reach.go` (export `LocalNATSURL`)

**Interfaces:**
- Produces:

```go
type ListenerConfig struct {
	MachineID  string
	NATSURLs   []string
	Port       int    // 0 means 9020
	HostBridge string // "" leaves ENVOY_HOST_BRIDGE unset
}

// ListenerEnv returns the listener's non-secret environment, KEY=value, in a fixed order.
func ListenerEnv(c ListenerConfig) ([]string, error)

const EnvNATSSeed = "NATS_NKEY_SEED" // its value is a secret; the component appends it
```

The contract implemented is the listener's own: `packages/envoy/internal/config/config.go:23-63`
(`ENVOY_MACHINE_ID` and `NATS_URLS` required, `PORT`), `cmd/listener/main.go:618`
(`ENVOY_HOST_BRIDGE`) and `internal/bus/reach.go:33-66` (`ENVOY_ALLOW_REMOTE_NATS=1` needed for
any URL that is not `localhost` or a loopback literal). Webhooks are not part of this plan: no
consumer here enables one (the existing Docker-host deployment sets none; the GCP hub has none in
the first version, Sami 2026-10-13). `ENVOY_LISTEN_HOST` is not offered either: a non-loopback
listener needs an API token (`cmd/listener/main.go:420-429`), which nothing here provides.

- [ ] **Step 1: export the reach rule.** Rename `localNATSURL` to `LocalNATSURL` in
`internal/bus/reach.go` (its callers are `reach.go:38` and its definition `:46-50`), keep the doc
comment, run `cd packages/envoy && go test ./internal/bus/`.

- [ ] **Step 2: failing table test.** The first row is the existing Docker-host container's
environment (the Docker-host program's `services.ts:75-85`).

```go
func TestListenerEnv(t *testing.T) {
	cases := []struct {
		name string
		in   core.ListenerConfig
		want []string
		err  string
	}{
		{
			name: "remote NATS, host bridge, default port",
			in:   core.ListenerConfig{MachineID: "example-host-a", NATSURLs: []string{"nats://hub.internal.example:4222"}, HostBridge: "127.0.0.1"},
			want: []string{"PORT=9020", "ENVOY_MACHINE_ID=example-host-a", "NATS_URLS=nats://hub.internal.example:4222", "ENVOY_ALLOW_REMOTE_NATS=1", "ENVOY_HOST_BRIDGE=127.0.0.1"},
		},
		{
			name: "loopback NATS needs no remote opt-in",
			in:   core.ListenerConfig{MachineID: "m", NATSURLs: []string{"nats://127.0.0.1:4222"}, Port: 9021},
			want: []string{"PORT=9021", "ENVOY_MACHINE_ID=m", "NATS_URLS=nats://127.0.0.1:4222"},
		},
		{
			name: "any remote URL in the list opts in",
			in:   core.ListenerConfig{MachineID: "m", NATSURLs: []string{"nats://localhost:4222", "nats://hub.internal.example:4222"}},
			want: []string{"PORT=9020", "ENVOY_MACHINE_ID=m", "NATS_URLS=nats://localhost:4222,nats://hub.internal.example:4222", "ENVOY_ALLOW_REMOTE_NATS=1"},
		},
		{name: "machine id required", in: core.ListenerConfig{NATSURLs: []string{"nats://127.0.0.1:4222"}}, err: "machineId is required"},
		{name: "a NATS URL required", in: core.ListenerConfig{MachineID: "m"}, err: "natsUrls needs at least one URL"},
		{name: "a comma in a URL is refused", in: core.ListenerConfig{MachineID: "m", NATSURLs: []string{"nats://a:1,nats://b:2"}}, err: "natsUrls[0] contains a comma"},
		{name: "an empty URL is refused", in: core.ListenerConfig{MachineID: "m", NATSURLs: []string{" "}}, err: "natsUrls[0] is empty"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := core.ListenerEnv(c.in)
			if c.err != "" {
				require.ErrorContains(t, err, c.err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, c.want, got)
		})
	}
}
```

Run `cd packages/envoy/infra && go test ./core/`; see it fail; implement with `bus.LocalNATSURL`
deciding the opt-in; see it pass.

- [ ] **Step 3: commit** `feat(envoy-infra): the listener's environment`.

### Task 1.3: `envoy-infra:host:Listener`

**Files:**
- Create: `packages/envoy/infra/host/listener.go`, `watchdog.go`, `listener_test.go`,
  `testdata/watchdog-create.golden`, `testdata/watchdog-delete.golden`
- Create: `packages/envoy/infra/host/live_test.go` (build tag `pulumilive`)
- Create: `packages/envoy/infra/internal/pulumitest/pulumitest.go` (the live harness)
- Modify: `packages/envoy/infra/provider/provider.go` (register), `provider_test.go`
- Modify: `.github/workflows/envoy-and-contracts.yaml` (`envoy-smoke` runs the live tests)

**Interfaces:**
- Consumes: `core.ListenerEnv`, `core.EnvNATSSeed`, `providers.Require`.
- Produces (token `envoy-infra:host:Listener`):

```go
type ListenerArgs struct {
	MachineID     string                  `pulumi:"machineId"`
	NatsUrls      pulumi.StringArrayInput `pulumi:"natsUrls"`               // may come from another stack's output
	Image         string                  `pulumi:"image"`                  // e.g. ghcr.io/sjawhar/legion/envoy:<tag>
	ContainerName *string                 `pulumi:"containerName,optional"` // default "envoy-listener"
	Port          *int                    `pulumi:"port,optional"`          // default 9020
	HostBridge    *string                 `pulumi:"hostBridge,optional"`
	Seed          pulumi.StringInput      `pulumi:"seed,optional" provider:"secret"`
	Watchdog      *WatchdogArgs           `pulumi:"watchdog,optional"`
	Adopt         *AdoptArgs              `pulumi:"adopt,optional"`
}

type WatchdogArgs struct {
	Host string `pulumi:"host"` // the SSH address only; pulumi-command logs in as root (v1.2.1 remote/connection.go:57); "@" and "://" are refused
}

// Names of root-level resources this listener now owns; each child gets
// pulumi.Alias{Name: <name>, NoParent: true}.
type AdoptArgs struct {
	Container *string `pulumi:"container,optional"`
	Image     *string `pulumi:"image,optional"`
	Watchdog  *string `pulumi:"watchdog,optional"`
}

type Listener struct {
	pulumi.ResourceState
	ContainerName pulumi.StringOutput `pulumi:"containerName"`
	Port          pulumi.IntOutput    `pulumi:"port"`
}
```

Resources, all with `pulumi.Parent(comp)`, registered only after `providers.Require(..., "docker")`
(and `"command"` when `watchdog` is set) succeed:

| Child name | Type | Args (exactly) | Options |
| --- | --- | --- | --- |
| `<name>-image` | `docker.RemoteImage` | `Name: image`, `KeepLocally: true` | alias from `adopt.image` |
| `<name>-container` | `docker.Container` | `Name: containerName`, `Image: image.ImageId`, `Command: ["/usr/local/bin/envoy-listener"]`, `Restart: "unless-stopped"`, `StopTimeout: 30`, `DestroyGraceSeconds: 30`, `NetworkMode: "host"`, `LogOpts: {"max-file": "5", "max-size": "20m"}`, `Envs`: `core.ListenerEnv` (computed with `ApplyT` over `natsUrls`), then `NATS_NKEY_SEED=<seed>` when `seed` is set, `Healthcheck: {Tests: ["CMD","curl","-f","http://127.0.0.1:<port>/healthz"], Interval: "10s", Timeout: "3s", Retries: 3, StartPeriod: "30s"}`, `Wait: true`, `WaitTimeout: 90` | `DeleteBeforeReplace(true)`, alias from `adopt.container` |
| `<name>-watchdog` (only with `watchdog`) | `remote.Command` | `Connection: {Host: watchdog.host}`; `Create` and `Delete`: the Docker-host program's texts (`watchdog.ts:121-162`), byte for byte, with the env file built from the container's envs, the image reference, the port in `healthcheck_cmd` (`watchdog.ts:40`) and the component's name in the closing `echo` (`:155`) | `DependsOn(container)`, alias from `adopt.watchdog` |

Refusals, each an error naming the input: `watchdog` with a `containerName` other than
`envoy-listener` (R5); `watchdog` with `seed` (R10); a `watchdog.host` containing `@` or `://`; these
before any registration. A `natsUrls` element `core.ListenerEnv` refuses fails the update, from the
`ApplyT` that computes the envs.

- [ ] **Step 1: failing unit tests** (`listener_test.go`) with `pulumi.WithMocks`, run through
`NewListener` directly:
  1. Defaults: the container's recorded inputs equal the table with `containerName =
     "envoy-listener"`, port 9020, and `Envs` as a set equal to
     `{"PORT=9020","ENVOY_MACHINE_ID=example-host-a","NATS_URLS=nats://hub.internal.example:4222","ENVOY_ALLOW_REMOTE_NATS=1","ENVOY_HOST_BRIDGE=127.0.0.1"}`
     for `MachineID: "example-host-a", NatsUrls: ["nats://hub.internal.example:4222"], HostBridge: "127.0.0.1"`.
  2. `natsUrls` given as an `Output` resolves into the same envs.
  3. A seed adds `NATS_NKEY_SEED=<seed>` and the recorded `envs` are secret.
  4. `adopt.container: "listener-example-host-a"` gives the container
     `Alias{Name: "listener-example-host-a", NoParent: true}` (read from the mock's registration).
  5. No docker provider: the run fails with `Require`'s message and the mocks saw no registration.
  6. `watchdog` with `containerName: "envoy-listener-b"` is refused; `watchdog` with `seed` is
     refused naming both inputs; `watchdog: {host: "ssh://example-host-a"}` and
     `{host: "agent@example-host-a"}` are refused.
  7. The watchdog's `create` and `delete` texts for component name `example-host-a`, port 9020 and
     no seed equal `testdata/watchdog-create.golden` and `testdata/watchdog-delete.golden`. Render
     the goldens once from the Docker-host program's `watchdog.ts` with the same envs and image and
     commit them with no header line (say in the commit message how they were produced). A second
     case with `port: 9021` changes the env file's `PORT=` line and the `healthcheck_cmd` line, and
     nothing else.
  Run `cd packages/envoy/infra && go test ./host/`; see them fail.

- [ ] **Step 2: implement** `NewListener(ctx *pulumi.Context, name string, args *ListenerArgs, opts ...pulumi.ResourceOption) (*Listener, error)`
in the shape of pulumi-go-provider's `examples/random-login/main.go:79-104`
(`ctx.RegisterComponentResource(p.GetTypeToken(ctx), name, comp, opts...)`), register it with
`infer.ComponentF(host.NewListener)` in `provider.New`, add the schema assertion. Run until green.

- [ ] **Step 3: the live harness** (`internal/pulumitest`): builds the plugin with
`go build -ldflags "-X main.version=0.0.1" -o <dir>/pulumi-resource-envoy-infra ./cmd/pulumi-resource-envoy-infra`,
writes a Pulumi YAML project into a temp dir whose `Pulumi.yaml` has
`plugins: {providers: [{name: envoy-infra, path: <dir>}]}`, and runs the `pulumi` CLI with
`PULUMI_BACKEND_URL=file://<tmp>`, `PULUMI_CONFIG_PASSPHRASE=test`,
`PULUMI_SKIP_UPDATE_CHECK=true`; exposes `Up`, `Preview` (`pulumi preview --json --show-sames`,
parsed into `steps[].op` and `steps[].urn`), `Export` (`pulumi stack export`) and `Destroy`; and
fails the test with the CLI's stderr on any non-zero exit. A live test with `ENVOY_SMOKE_IMAGE`
unset calls `t.Fatal` naming the build-and-run command, as `packages/envoy/internal/smoke/listener_test.go:31-33`
does.

- [ ] **Step 4: live tests** (`host/live_test.go`, `//go:build pulumilive`), each program using a
`pulumi:providers:docker` resource with `options.version: 4.11.0` (the version the first consumer
pins) passed in `options.providers`:
  1. **Runs a listener.** `testnats.Start`; apply an `envoy-infra:host:Listener` (`image:
     ${ENVOY_SMOKE_IMAGE}`, `natsUrls: [<testnats loopback url>]`, `port: <free port>`,
     `containerName: envoy-listener-live-<random>`); require `GET http://127.0.0.1:<port>/healthz`
     to answer `"status":"healthy"` within 60 s; `Export` shows the container's `provider` is the
     explicit docker provider's URN; destroy; the container is gone.
  2. **Adoption previews clean.** Apply program A: root-level `docker:RemoteImage envoy-image-live`
     and `docker:Container listener-live` with exactly the table's inputs. Preview program B: the
     same provider and an `envoy-infra:host:Listener live` with `adopt: {container: listener-live,
     image: envoy-image-live}` and the same values. Require: the child URNs `…$docker:index/container:Container::live-container`
     and `…::live-image` appear with op `same`; the only non-`same` step is the component's
     `create`; no `delete` of the old root URNs.
  3. **Seed reaches NATS.** With `testnats.StartNkeyAuthorized(t, <public key>)`: a listener with
     `seed` reaches `"status":"healthy"`; one without it never does within 60 s, and its log shows
     the authorization violation.
  4. **Go consumers can use the SDK.** In a scratch Go program, `pulumi package add <binary>`, then
     `go build ./...` succeeds.

- [ ] **Step 5: CI.** In `envoy-smoke`, after the container smoke test: install the Pulumi CLI
v3.268.0 from `https://get.pulumi.com/releases/sdk/pulumi-v3.268.0-linux-x64.tar.gz`, verifying
SHA-256 `903cafeaa9f6327e4bf53a8b7b99ae660f26198acfa804a2f982bf5de3f1de0b` before extracting;
download `pulumi-resource-docker-v4.11.0-linux-amd64.tar.gz` from pulumi-docker's v4.11.0 release,
verify it against the SHA-256 in that release's checksums file (written into the workflow as a
literal), and `pulumi plugin install resource docker v4.11.0 --file <tarball>`; then in
`packages/envoy/infra`: `go test -tags pulumilive -v -timeout 15m ./host/`, and
`go build -ldflags "-X main.version=1.2.3" -o /tmp/pr ./cmd/pulumi-resource-envoy-infra && pulumi package get-schema /tmp/pr | jq -e '.version == "1.2.3"'`.
Run `.github/scripts/check-image-trigger-paths.sh` and `.github/scripts/check-action-pins.sh`.

- [ ] **Step 6: commit** `feat(envoy-infra): host:Listener`.

### Task 1.4: release

**Files:**
- Create: `.github/envoy-infra-targets.json` (multi-line, like `.github/go-release-targets.json`):
  `{"envoy-infra": ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]}`
- Create: `.github/scripts/check-envoy-infra-release-paths.sh` (R8)
- Modify: `.github/workflows/release.yaml` (a `changes` filter `envoy_infra`, a job `envoy_infra`)
- Modify: `.github/workflows/release-envoy-listener.yaml` (R3)
- Modify: `.github/workflows/pr-and-main.yaml` (notices for the new binary, cache key)

- [ ] **Step 1: release paths (R8).** The paths are `packages/envoy/infra/`, every
`packages/envoy/internal/<pkg>/` directory the plugin compiles in, `packages/envoy/go.mod`,
`packages/envoy/go.sum`, `scripts/go-third-party-notices.sh`, `.github/envoy-infra-targets.json`.
`check-envoy-infra-release-paths.sh` runs `go list -deps -f '{{.ImportPath}}' ./cmd/...` in
`packages/envoy/infra`, keeps `github.com/sjawhar/envoy/internal/*`, and fails naming any directory
missing from the `envoy_infra` filter or the bump paths; `envoy-infra-go` runs it.

- [ ] **Step 2: the job**, shaped like `legion` (`release.yaml:206-386`): runs on `main` when the
`envoy_infra` filter matched or on `workflow_dispatch`; ordered after the jobs that push a bump
commit (as `pi_legion` after `pi_envoy`, `:696-707`); permissions `contents: write`,
`id-token: write`, `attestations: write`; checkout with the deploy key and full history; version:

```bash
PREV_TAG=$(git tag -l 'envoy-infra-v*' --sort=-v:refname | head -1)
if [ -z "$PREV_TAG" ]; then PREV_VERSION=0.0.0; RANGE=HEAD; else PREV_VERSION="${PREV_TAG#envoy-infra-v}"; RANGE="${PREV_TAG}..HEAD"; fi
VERSION=$(.github/scripts/release-bump.sh "$PREV_VERSION" "$RANGE" \
  --patch-path scripts/go-third-party-notices.sh --patch-path .github/envoy-infra-targets.json \
  -- <the paths of Step 1>)
```

On `empty` or `none` it skips with the same `::notice` and step-summary line the `legion` job
writes (`:286-297`). Build per target with `CGO_ENABLED=0` and `-ldflags "-X main.version=$VERSION"`,
write `THIRD_PARTY_NOTICES` with `scripts/go-third-party-notices.sh`, pack
`pulumi-resource-envoy-infra-v$VERSION-$os-$arch.tar.gz` with the binary and notices at the archive
root. Smoke the linux/amd64 binary with the pinned Pulumi CLI:
`pulumi package get-schema <binary> | jq -e --arg v "$VERSION" '.version == $v and .pluginDownloadURL == "https://github.com/sjawhar/legion/releases/download/envoy-infra-v${VERSION}"'`.
Attest provenance, push the tag, `gh release create "envoy-infra-v$VERSION" assets/* --verify-tag --generate-notes --title "envoy-infra v$VERSION"`.
Add the filter and the workflow's `on.push.paths`.

- [ ] **Step 3: R3.** In `release-envoy-listener.yaml` add `- "!packages/envoy/infra/**"` after
`- "packages/envoy/**"` and `':(exclude)packages/envoy/infra'` after `--` in the bump. Run
`.github/scripts/check-image-trigger-paths.sh`.

- [ ] **Step 4: notices.** In `pr-and-main.yaml`'s notices step add a loop over
`jq -r '.["envoy-infra"][]' .github/envoy-infra-targets.json` writing notices for
`./cmd/pulumi-resource-envoy-infra` from `packages/envoy/infra`; add `packages/envoy/infra/go.sum`
to the cache key (`:115`). Run that step's commands locally.

- [ ] **Step 5: rehearse the job** by running its shell steps locally with `VERSION=0.0.1`, up to
but not including the tag push: four tarballs, each `tar tzf` listing exactly
`pulumi-resource-envoy-infra` and `THIRD_PARTY_NOTICES`, and the schema smoke passing.

- [ ] **Step 6: commit** `ci(envoy-infra): release the plugin`.

### Task 1.5: documentation

**Files:** Create `packages/envoy/infra/README.md`, `packages/envoy/infra/AGENTS.md`; modify
`packages/envoy/AGENTS.md` ("Where to look"), root `AGENTS.md` (WHERE TO LOOK),
`docs/site/src/content/docs/dispatch/envoy.md` ("Running the listener": "With Pulumi").

- [ ] Write: what the plugin deploys; `pulumi package add envoy-infra@<v> --server 'https://github.com/sjawhar/legion/releases/download/envoy-infra-v${VERSION}'`;
  `host:Listener`'s inputs with a TypeScript and a YAML example (placeholders only); the
  providers rule; `adopt` and the preview that proves a move; the module's own `go.work` (R1);
  how to run the unit and live tests. Use `updating-docs`. Run `bunx biome check .` and
  `.github/scripts/check-private-names.sh`. Commit `docs(envoy-infra): host:Listener`.

### Task 1.6: production check, after merge

- [ ] Once #1946 is merged and `release.yaml` has run: record the run id and the
  `envoy-infra-v<v>` tag; from an empty directory, `pulumi package add envoy-infra@<v> --server 'https://github.com/sjawhar/legion/releases/download/envoy-infra-v${VERSION}'`
  succeeds, and `pulumi preview` of a YAML program with one explicit `pulumi:providers:docker` and
  one `envoy-infra:host:Listener` plans creates of exactly the stack, that provider, the component,
  its `docker:index/remoteImage:RemoteImage` and its `docker:index/container:Container`. Record the
  commands and output on #1946.

---

## Slice 3: NATS authorization, the seed command, `gcp:NatsHub`

Branch `envoy-infra-gcp` in its own clone (`~/.worktrees/legion/envoy-infra-gcp`), stacked on
`envoy-infra` with gh-stack. Task 3.1 starts once Task 1.1 is on `envoy-infra`; 3.2 needs 3.1; 3.3
needs 3.2; 3.4 needs 3.1 and 3.2; 3.5 needs 3.3 and 3.4.

### Task 3.1: NATS authorization and server configuration (`core`)

**Files:**
- Create: `packages/envoy/infra/core/nats_auth.go`, `nats_auth_test.go`, `nats_config.go`,
  `nats_config_test.go`, `testdata/nats-server.conf.golden`
- Modify: `.github/workflows/envoy-and-contracts.yaml`: `envoy-infra-go`'s pre-pull also pulls
  `const HubImage`.
- Modify: `packages/envoy/internal/testnats/nats.go`: export `startNkeyConfig` as
  `StartNkeyConfig` (callers `:220`, `:238`); add `const HubImage =
  "mirror.gcr.io/library/nats:2.15.1-alpine@sha256:2ae57dfba3063d5e25fecb75256118c0882a1a2d2cf7d0fba17a7c701df956f6"`
  on one line beside `Image`; add `StartImageConfig(t, image, config string, anonymous bool)`, which
  runs `image` with the config mounted at `/etc/nats/nats-server.conf` and the argument
  `-c /etc/nats/nats-server.conf`, waiting with the existing `answering` check when `anonymous` is
  false and for a credential-less connect to succeed when it is true.

**Interfaces:**

```go
type NATSUser struct {
	Name        string // [a-z0-9-]+; used in errors and secret names
	PublicKey   string // nkey user public key, "U..."
	Permissions Permissions
}

type Permissions struct {
	Publish   Rules `json:"publish"`
	Subscribe Rules `json:"subscribe"`
}

type Rules struct {
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

type NATSServerConfig struct {
	ServerName string
	ClientPort int    // 4222
	HTTPPort   int    // 8222
	StoreDir   string // "/data"
	Users      []NATSUser
	NoAuthUser string // "" for none; else a Name in Users
}

func (c NATSServerConfig) Validate() error
func (c NATSServerConfig) Render() (string, error) // nats-server configuration text
```

Rendering: each user is one JSON object
`{"nkey":"U…","permissions":{"publish":{"allow":[…],"deny":[…]},"subscribe":{…}}}` inside
`authorization { users: [ … ] }`, encoded with `json.Encoder.SetEscapeHTML(false)` so `>` stays raw
and with no `\u` escapes (nats-server's configuration strings accept raw UTF-8 and `\"`, `\\`,
`\t`, `\n`, `\r`, `\xXX`); `no_auth_user` is top level, never inside `authorization`, naming the
fallback user's public key; `jetstream { store_dir: "<StoreDir>" }`, `listen`, `http_port`,
`server_name`. Validation refuses, naming the user and the field: no users and no `NoAuthUser`
(R7); a public key `nkeys.IsValidPublicUserKey` rejects; duplicate names or keys; a name outside
`[a-z0-9-]+`; a subject that is empty, contains whitespace or a control character, has an empty
dot token, has `*` or `>` as part of a token, or has `>` anywhere but last; a publish rule with a
space; a subscribe rule with more than one space (`"<subject> <queue>"` is allowed); a
`NoAuthUser` that names no user.

- [ ] **Step 1: failing tests.** Validation table, one row per refusal; a golden render for two
  users and a `no_auth_user`.
- [ ] **Step 2: failing real-server tests**, each run twice: on `testnats.Image` (2.10) with
  `StartNkeyConfig`, and on the hub image pinned by digest
  (`mirror.gcr.io/library/nats:2.15.1-alpine@sha256:2ae57dfba3063d5e25fecb75256118c0882a1a2d2cf7d0fba17a7c701df956f6`)
  with `StartImageConfig`: a client authenticating with user A's seed (`nkeys.CreateUser`)
  connects; a client with no credential is refused when `NoAuthUser` is empty; user A publishing
  outside its allow list gets a permissions violation; with `NoAuthUser` set (on 2.10 via
  `StartImageConfig(t, testnats.Image, config, true)`, on the hub image via
  `StartImageConfig(t, testnats.HubImage, config, true)`), a credential-less client connects with
  that user's permissions.
- [ ] **Step 3: implement**, run `cd packages/envoy && go test ./internal/testnats/` and
  `cd packages/envoy/infra && go test ./core/`. Commit
  `feat(envoy-infra): NATS authorization and server configuration`.

### Task 3.2: the listener's and a session's permission sets (`core`)

**Files:**
- Create: `packages/envoy/infra/core/permissions.go`, `permissions_test.go`,
  `core/permissions_live_test.go` (build tag `natslive`)
- Modify: `packages/envoy/internal/bus` (exported `ListenerDurable(machineID string) string` and
  `ListenerQueue(machineID string) string` for `"listener-" + machineID` with spaces as `-`
  (`cmd/listener/main.go:531`) and `"envoy-listener-" + machineID` (`:634`)), `cmd/listener/main.go`
  (calls them), `.github/workflows/envoy-and-contracts.yaml` (the natslive run, Step 2)

**Interfaces:**

```go
func ListenerPermissions(machineID string) Permissions // the grant envoy-listener needs
func SessionPermissions() Permissions                  // the grant any agent session needs
func HostPermissions(machineID string) Permissions     // their union: one NATS user per host
```

Built from Envoy's exported names: `bus.Stream`, `bus.StreamSubjects()`, `bus.ListenerDurable`,
`bus.ListenerQueue`, `store.Bucket`, `store.RoleBucket`, `session.SessionBucket`, `cistore.Bucket`,
`contracts.AgentStreamFramesSubject` and `AgentStreamControlSubject` with `*` for the session id.
The listener's set: publish the stream subjects, `notifications.role.>`,
`notifications.envoy.exceptions.notifications.>`, `$JS.API.STREAM.{INFO,CREATE,UPDATE}.<stream>`,
`$JS.API.CONSUMER.{INFO,CREATE}.<stream>.<durable>`, `$JS.ACK.<stream>.<durable>.>`, `$JS.API.INFO`,
and for each bucket `$JS.API.STREAM.{INFO,CREATE}.KV_<b>`, `$JS.API.DIRECT.GET.KV_<b>.>`,
`$JS.API.STREAM.MSG.GET.KV_<b>`, `$KV.<b>.>`, `$JS.API.CONSUMER.{CREATE,DELETE}.KV_<b>.>`,
`$JS.FC.KV_<b>.>`, plus `$JS.API.STREAM.PURGE.KV_envoy_interests`; subscribe `_INBOX.>` and
`notifications.role.> <queue>`. A session's: publish `_INBOX.>`, `agentstream.*.frames`; subscribe
`notifications.>`, `agentstream.*.control`. (This grant was measured live in an existing
deployment; the live test below re-proves it against this branch's listener.)

- [ ] **Step 1: failing unit tests:** each set exactly for `example-host-a`; `ListenerDurable` and
  `ListenerQueue` for `example-host-a` and for `example host` (a space) equal the strings the
  listener used before the move. The listener tests that spell the strings
  (`main_test.go:2260,2277`, `reconnect_shutdown_test.go:158`, `shutdown_test.go:113`,
  `startup_test.go:51`) keep their literals as independent checks.
- [ ] **Step 2: failing live test** (`natslive`), on both images as in Task 3.1: one user holding
  `HostPermissions("example-host-a")`; build and run `packages/envoy/cmd/listener` with its seed
  (`NATS_NKEY_SEED`), `ENVOY_MACHINE_ID=example-host-a` and the server's loopback URL; require
  `/healthz` to answer `"status":"healthy"` within 60 s (the durable bound under the grant); a
  session client with the same seed subscribes to `notifications.agent.s1` and publishes to
  `agentstream.s1.frames` with no permissions error; a message `POST`ed for `s1` to the listener's
  `/v1` API reaches the session. Negative control: the same run with `$JS.API.CONSUMER.CREATE…`
  removed from the grant never reaches "healthy" and the server logs a permissions violation. Add
  `go test -tags natslive ./core/` to `envoy-infra-go`.
- [ ] **Step 3: implement**, pass, commit `feat(envoy-infra): permission sets from Envoy's own subjects`.

### Task 3.3: `envoy-infra seed`

**Files:** Modify `.github/workflows/envoy-and-contracts.yaml` (`envoy-infra-go`'s natslive run
becomes `go test -tags natslive ./core/ ./seed/`). Create `packages/envoy/infra/cmd/envoy-infra/main.go`, `packages/envoy/infra/seed/seed.go`,
`seed_test.go`, `seed/gcpstore.go`, `gcpstore_test.go`, `seed/seed_live_test.go` (build tag `natslive`)

**Interfaces:**

```go
type Store interface {
	Versions(ctx context.Context, name string) (int, error) // every state, destroyed included
	Add(ctx context.Context, name string, value []byte) (version string, err error)
	Access(ctx context.Context, name, version string) ([]byte, error)
}

// Generate writes a new user seed into an existing secret with no versions and returns its public key.
func Generate(ctx context.Context, s Store, name string) (publicKey string, err error)
// Verify checks that the secret's latest version derives publicKey.
func Verify(ctx context.Context, s Store, name, publicKey string) error

var ErrRefused = errors.New("seed: refused") // nothing was written: exit 2
// Any other error after Add returned is a post-write error: a version may exist: exit 1.
```

CLI: `envoy-infra seed generate --store gcp-secret-manager --name projects/<p>/secrets/<s>` prints
only the public key; `envoy-infra seed verify --store gcp-secret-manager --name … --public-key U…`
prints `ok`. The seed never reaches stdout, stderr, a log or an error message. The GCP store uses
Application Default Credentials or `GOOGLE_OAUTH_ACCESS_TOKEN`.

- [ ] **Step 1: failing tests** with a fake `Store`: refuses a secret with any version (exit 2,
  nothing added); refuses a missing secret (exit 2); after `Add`, re-reads the written version and
  checks it derives the printed key; a canary seed never appears in any error or output; `Verify`
  passes for the right key and fails naming the secret for another.
- [ ] **Step 2: the spec's seed acceptance** (`seed_live_test.go`): `Generate` against the fake
  store; render a `core` config with the returned key; start it with `testnats.StartNkeyConfig`; a
  client using the seed the fake store holds connects, and one without it is refused.
- [ ] **Step 3: GCP store** over `cloud.google.com/go/secretmanager/apiv1` v1.22.0
  (`ListSecretVersions` counting every state, `AddSecretVersion`, `AccessSecretVersion`; confirm
  the method names in v1.22.0's source first). Its unit test runs against a gRPC fake server
  implementing those three RPCs.
- [ ] **Step 4: commit** `feat(envoy-infra): seed command`.

### Task 3.4: `envoy-infra:gcp:NatsHub`

**Files:** Create `packages/envoy/infra/gcp/natshub.go`, `cloudconfig.go`, `cloudconfig_test.go`,
`natshub_test.go`, `fetch_key_test.go`, `testdata/cloud-config.golden`

**Interfaces:**

```go
type NatsHubArgs struct {
	Users          []NATSUserArgs     `pulumi:"users"`
	NoAuthUser     *string            `pulumi:"noAuthUser,optional"`
	TailnetAuthKey pulumi.StringInput `pulumi:"tailnetAuthKey" provider:"secret"`
	Hostname       string             `pulumi:"hostname"`    // ^[a-z][a-z0-9-]{1,25}$
	Tags           []string           `pulumi:"tags"`        // "tag:<name>"
	Zone           string             `pulumi:"zone"`
	MachineType    *string            `pulumi:"machineType,optional"` // default "e2-small"
	DiskSizeGb     *int               `pulumi:"diskSizeGb,optional"`  // default 20
	NatsImage      string             `pulumi:"natsImage"`            // must contain "@sha256:"
	TailscaleImage string             `pulumi:"tailscaleImage"`       // must contain "@sha256:"
	Network        *string            `pulumi:"network,optional"`     // created when absent
	Subnetwork     *string            `pulumi:"subnetwork,optional"`
}

type NATSUserArgs struct {
	Name        string           `pulumi:"name"`
	PublicKey   string           `pulumi:"publicKey"`
	MachineID   *string          `pulumi:"machineId,optional"`   // permissions = core.HostPermissions(machineId)
	Permissions *PermissionsArgs `pulumi:"permissions,optional"` // explicit; exactly one of the two
}

type PermissionsArgs struct {
	Publish   RulesArgs `pulumi:"publish"`
	Subscribe RulesArgs `pulumi:"subscribe"`
}

type RulesArgs struct {
	Allow []string `pulumi:"allow,optional"`
	Deny  []string `pulumi:"deny,optional"`
}

type NatsHub struct {
	pulumi.ResourceState
	NatsUrl       pulumi.StringOutput `pulumi:"natsUrl"`       // nats://<hostname>:4222
	InstanceGroup pulumi.StringOutput `pulumi:"instanceGroup"` // the group's name
}
```

`users` and `noAuthUser` map onto `core.NATSServerConfig` (`ClientPort: 4222`, `HTTPPort: 8222`,
`StoreDir: "/data"`, `ServerName: hostname`), validated and rendered before any registration. A
user names `machineId` (the usual case: no consumer spells Envoy's subjects) or `permissions`;
both or neither is refused, naming the user.

Resources (pulumi-gcp v10.1.0; re-check each field list below against that tag before use), all
children, after `providers.Require(..., "gcp")`:
- `serviceaccount.Account` `AccountId: <hostname>-hub` (6-30 characters and RFC 1035, which the
  hostname rule guarantees).
- `secretmanager.Secret` `SecretId: <hostname>-tailnet-auth-key`, `Replication: {Auto: {}}`;
  `SecretVersion` from `tailnetAuthKey`; `SecretIamMember` giving the account
  `roles/secretmanager.secretAccessor` on it.
- `compute.Network` (`AutoCreateSubnetworks: true`) only when `network` is absent.
- `compute.Firewall`: tcp/8222 from `130.211.0.0/22` and `35.191.0.0/16` to the VM tag
  `<hostname>-hub`, nothing else.
- `compute.HealthCheck`: HTTP 8222 `/healthz`, `CheckIntervalSec: 10`, `UnhealthyThreshold: 6`.
- `compute.InstanceTemplate`: `NamePrefix: <hostname>-`, boot disk
  `projects/cos-cloud/global/images/family/cos-stable`, a data disk `DeviceName: "data"`,
  `Boot: false`, `AutoDelete: false`, `DiskSizeGb`, an empty `AccessConfigs` entry for egress, the
  account with `cloud-platform` scope, the VM tag, `Metadata: {"user-data": <cloud-config>}`.
- `compute.InstanceGroupManager`: `BaseInstanceName: <hostname>`, `Zone`, `TargetSize: 1`,
  `Versions: [{InstanceTemplate: template.SelfLinkUnique}]`,
  `StatefulDisks: [{DeviceName: "data", DeleteRule: "NEVER"}]`,
  `AutoHealingPolicies: {HealthCheck, InitialDelaySec: 300}`,
  `UpdatePolicy: {Type: "PROACTIVE", MinimalAction: "REPLACE", ReplacementMethod: "RECREATE", MaxSurgeFixed: 0, MaxUnavailableFixed: 1}`,
  `WaitForInstances: true`, `WaitForInstancesStatus: "STABLE"`.

Cloud-config (`cloudconfig.go`, `gopkg.in/yaml.v3`, re-run on every COS boot): `bootcmd`:
`test -c /dev/net/tun || modprobe tun`; format `/dev/disk/by-id/google-data` only when `blkid`
finds no filesystem; mount it at `/mnt/disks/data`; create `nats/` and `tailscale/` under it;
create `/run/envoy` (mode 0700) and an empty `/run/envoy/tailnet-auth-key` (mode 0600), so the
container's bind mount is always a file.
`write_files`: `/etc/nats/nats-server.conf`; `/usr/local/bin/fetch-tailnet-key` writing the key to
`/run/envoy/tailnet-auth-key` (mode 0600) from Secret Manager with the metadata server's token,
taking the metadata host from `GCE_METADATA_HOST` and the Secret Manager base URL from
`SECRET_MANAGER_ENDPOINT` (defaults `metadata.google.internal` and
`https://secretmanager.googleapis.com`) and the output path as its first argument; units
`fetch-tailnet-key` (oneshot), `tailscaled`
(`docker run --rm --name tailscaled --network host --cap-add NET_ADMIN --device /dev/net/tun -v /mnt/disks/data/tailscale:/var/lib/tailscale -v /run/envoy/tailnet-auth-key:/run/envoy/tailnet-auth-key:ro -e TS_AUTHKEY=file:/run/envoy/tailnet-auth-key -e TS_AUTH_ONCE=true -e TS_STATE_DIR=/var/lib/tailscale -e TS_USERSPACE=false -e TS_HOSTNAME=<hostname> -e TS_EXTRA_ARGS=--advertise-tags=<tags> <tailscaleImage>`;
containerboot hands the path to the tailscale CLI inside the container, tailscale v1.104.1
`cmd/containerboot/tailscaled.go:120-121`)
and `nats`
(`docker run --rm --name nats --network host -v /mnt/disks/data/nats:/data -v /etc/nats/nats-server.conf:/etc/nats/nats-server.conf:ro <natsImage> -c /etc/nats/nats-server.conf`,
`ExecStop=/usr/bin/docker stop -t 30 nats`), each with `RequiresMountsFor=/mnt/disks/data`;
`tailscaled` also has `After=` and `Wants=fetch-tailnet-key.service` (not `Requires=`: a node
whose tailnet state is on the disk never reads the key, so a failed fetch on a later boot must not
keep it down); `fetch-tailnet-key` retries up to five times, ten seconds apart, and has
`RemainAfterExit=yes`.
`runcmd`: `systemctl daemon-reload`, then `systemctl start` each unit (never `enable`: COS's `/etc`
is stateless). The golden pins the bootcmd lines and the ordering.

- [ ] **Step 1: failing tests.** Cloud-config golden; `fetch_key_test.go` runs the script against
  an `httptest` server for both endpoints and requires the key file with mode 0600, nothing of the
  key on stdout or stderr, and a non-zero exit on a 403; mocks: every resource's recorded inputs
  equal the list, the auth key's `SecretVersion` input is secret, no gcp provider refuses, an
  invalid user or an image without a digest refuses before any registration, `natsUrl` is
  `nats://<hostname>:4222`.
- [ ] **Step 2: implement, register with `infer.ComponentF(gcp.NewNatsHub)`, pass.** Commit
  `feat(envoy-infra): gcp:NatsHub`.

### Task 3.5: release the CLI, documentation

- [ ] Extend the `envoy_infra` job: also build `./cmd/envoy-infra` per target into
  `envoy-infra-$os-$arch.tar.gz` holding one directory `envoy-infra-$os-$arch/` with `envoy-infra`
  and `THIRD_PARTY_NOTICES` (the `legion` layout, for a mise `github:` pin); notices loop for it in
  `pr-and-main.yaml`.
- [ ] README and site: `gcp:NatsHub`'s inputs with a YAML example (placeholders); the seed workflow
  (Pulumi creates the secret empty, `envoy-infra seed generate` writes it, the public key goes into
  `users`); the permission sets; what the VM runs and why the stateful disk keeps its tailnet
  identity; the refusal of an empty user list. Commit `docs(envoy-infra): NATS hub and seeds`.

### Task 3.6: pre-merge proof in the first GCP deployment

- [ ] The slice's acceptance (E8, E9) runs in the first GCP deployment's repository, per its plan,
  against this branch's built plugin and CLI. That plan's tester records the result there; this PR
  gets a summary of outcomes and durations with placeholders only (no project, tailnet, host name
  or address).

---

## Hardening ledger

Items to verify, each with the row that settles it and where its result goes:

- `maxSurgeFixed: 0` with `maxUnavailableFixed: 1` on a zonal group of one (E9; the first GCP
  deployment's PR).
- `/dev/net/tun` on COS without `modprobe` (E9; same).
- The stateful policy end to end (E9; same).
- A v4 docker provider diffing unset v5-only inputs (E3 in CI, E4 on the existing deployment).

## End-to-end verification plan

| Scenario | Slice | Class | Surface and transport | Identity and grant | Driver |
| --- | --- | --- | --- | --- | --- |
| E1. Unit, vet, notices | 1, 3 | infrastructure | GitHub Actions `envoy-infra-go`, `lint` | repo CI | Tasks 1.1, 1.4 |
| E2. `host:Listener` runs a listener that reaches "healthy", with the explicit provider | 1 | infrastructure | `pulumi up` of a YAML program against the runner's Docker, listener image built from the branch | CI runner | Task 1.3 live tests 1, 3 |
| E3. Adoption previews clean | 1 | infrastructure | `pulumi preview --json --show-sames` against the runner's Docker | CI runner | Task 1.3 live test 2 |
| E4. An existing Docker-host deployment moves onto `host:Listener` with no cloud change | 1 (pre-merge), then its consumer's slice | infrastructure | that deployment's own stack and backend, previewed with the branch's plugin binary | its operator, with the operator's passphrase (a human touch) | the consumer's plan; #1946 gets operation counts by type only |
| E5. The release job's assets and schema, and `pulumi package add` from the release | 1 | infrastructure | the job's shell rehearsed locally; then the real run and Task 1.6 | maintainer | Tasks 1.4, 1.6 |
| E6. Rendered authorization admits and refuses as designed, on 2.10 and on the hub image | 3 | infrastructure | real nats-server containers | CI runner | Task 3.1 |
| E7. The listener reaches "healthy" under `HostPermissions`, and a session under the same user receives | 3 | infrastructure | real nats-server and this branch's listener binary | CI runner | Task 3.2 live test |
| E8. Seed generate and verify against Secret Manager; a generated seed connects | 3 | infrastructure | in-repo fake store and real nats-server (Task 3.3); Secret Manager in the first GCP deployment | that deployment's operator | Task 3.3; Task 3.6 |
| E9. `gcp:NatsHub` serves NATS over the tailnet, admits a seeded client, refuses one without, keeps its tailnet identity and stream across `recreate-instances` | 3 | infrastructure | the first GCP deployment, applied from the branch's plugin | its operator | Task 3.6 |

Legion's own end-to-end stages (`scripts/e2e/stage*.sh`) exercise none of the changed paths. The
Envoy changes (`bus.LocalNATSURL`, `bus.ListenerDurable`/`ListenerQueue`,
`testnats.StartNkeyConfig`) are covered by `envoy-go` and `envoy-smoke`, which run on these PRs.

## Skill catalog

Skills available (listed from the directories): Legion `skills/` (`ce-simplify-code`,
`thermonuclear-code-quality`, `thermonuclear-deep-review`, `envoy`, `legion-*`, `dispatch*`); the
dotfiles skills (`opening-a-pr`, `landing-a-pr`, `using-docker`, `updating-docs`,
`review-judgment`, `analyze`, `gh-stack`); superpowers (`test-driven-development`,
`verification-before-completion`, `systematic-debugging`).

| Step | Worker | Required skills |
| --- | --- | --- |
| Tasks 1.1-1.4, 3.1-3.4 | `deep` (implementer) | `test-driven-development`, `using-docker`, `verification-before-completion`, `systematic-debugging` when a test fails unexpectedly |
| Tasks 1.5, 3.5 | `deep` | `updating-docs` |
| Opening and landing each PR; Task 1.6 | `deep` (implementer) | `opening-a-pr`, `landing-a-pr`, `gh-stack` (slice 3) |
| Slice acceptance | `deep` (tester) | `verification-before-completion`, `using-docker` |
| Task and final review | `reviewer` | `review-judgment` |
| Final pass per PR | coordinator | `ce-simplify-code` (once), then `thermonuclear-deep-review`, `thermonuclear-code-quality` |

## Contract change census

New contracts (component schemas, the CLI) tighten nothing existing. Changes to existing ones:

- `bus.localNATSURL` → `bus.LocalNATSURL`: `git grep -n localNATSURL -- packages/` → `reach.go:38,46,50`
  only. Rename, behaviour unchanged.
- The listener's durable and queue names move into `bus.ListenerDurable`/`ListenerQueue`:
  `git grep -n -e '"listener-"' -e '"envoy-listener-"' -- packages/envoy` → `cmd/listener/main.go:531,634`
  (now call the functions) and the tests `main_test.go:2260,2277`, `reconnect_shutdown_test.go:158`,
  `shutdown_test.go:113`, `startup_test.go:51` (keep their literals as independent checks). The
  names produced are unchanged (Task 3.2 Step 1).
- `testnats.startNkeyConfig` → `StartNkeyConfig`: `nats.go:220,238,315`.
- `release-envoy-listener.yaml` no longer runs for `packages/envoy/infra/**`, and its bump ignores
  it: `git grep -n 'packages/envoy' -- .github/` shows no `workflow_run` or other dependent of that
  workflow. Rollout: immediate.

## Permission source census

| Permission | Holder | Granted by | Enforced by | Test that fails without a holder |
| --- | --- | --- | --- | --- |
| NATS publish/subscribe for a host's listener and sessions | the host's nkey user | the consumer's `users` input to `gcp:NatsHub`, rendered by `core` | nats-server `authorization` | Task 3.2 live test and its negative control |
| `roles/secretmanager.secretAccessor` on the hub's tailnet auth key | the hub VM's service account | `gcp:NatsHub`'s `SecretIamMember` | Secret Manager IAM | E9 (the VM joins only if it reads the key) |
| Writing a host's seed secret | the operator running `envoy-infra seed generate` | the operator's project role (the consumer's plan names it) | Secret Manager IAM | E8 |
| `contents: write`, `id-token: write`, `attestations: write`, deploy-key tag push | the `envoy_infra` release job | the job's `permissions` block and `secrets.DEPLOY_KEY` | GitHub | E5 (the first release run) |
| Root on a watchdog host (writes `/etc/envoy`, `/usr/local/bin`, systemd units) | `root` at the consumer's `watchdog.host`, pulumi-command's default user | that host's SSH configuration | the host | E4 when the existing deployment has a watchdog; otherwise not exercised |

## Evidence

Pins and calls come from research notes made 2026-10-13/14 and the plan gate's checks:
pulumi-go-provider v1.7.0 `infer/component.go:71-167`, `provider_builder.go:74-80, 219-222`;
Pulumi SDK v3.265.0 `sdk/go/pulumi/provider.go:41-205`, `context.go:2156-2200`; Pulumi CLI v3.268.0
`sdk/go/common/workspace/plugins.go:216-218, 660-664, 753-781`, `pkg/pluginstorage/content.go:66-80`,
`pkg/cmd/pulumi/packagecmd/package_add.go:268-273`, `pkg/backend/display/display.go:317-326`,
`pkg/resource/deploy/step_generator.go:2885-2970`; pulumi-docker v4.11.0→v5.2.0 schema diff;
pulumi-gcp v10.1.0/v10.1.1 `go.mod`; tailscale v1.104.1 `cmd/containerboot/main.go`; nats-server
v2.15.1 `server/auth.go:328-337`, `server/opts.go:1235-1277`.
