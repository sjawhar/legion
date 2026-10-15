# Envoy infrastructure components: implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** a Go Pulumi plugin, `envoy-infra`, that deploys Envoy's NATS server and listeners, so a
new deployment needs configuration rather than infrastructure code of its own.

**Architecture:** a new Go module at `packages/envoy/infra` (module path
`github.com/sjawhar/envoy/infra`) builds `pulumi-resource-envoy-infra`, an executable plugin made
with `pulumi-go-provider`'s `infer` package, and in slice 2 an `envoy-infra` CLI. A `core` package
holds cloud-free logic (listener environment, NATS authorization, permission sets). Component
packages (`host`, `aws`, `gcp`) hold Pulumi components whose tokens are `envoy-infra:<package>:<Type>`.
The plugin is released from its own workflow, `release-envoy-infra.yaml`, as tarballs under the tag `envoy-infra-v<version>`, and
the schema names an `https` download URL with `${VERSION}`.

**Tech stack:** Go 1.26.8, pulumi-go-provider v1.7.0 (requires Pulumi `sdk/v3` and `pkg/v3`
v3.265.0), pulumi-docker SDK v5.2.0, pulumi-command SDK v1.2.1, pulumi-aws SDK v7 (slice 3), pulumi-gcp SDK v10.1.0 (slice 4;
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
  replaces or deletes no cloud resource and its stack outputs are unchanged (Sami, 2026-10-13). The
  only creates allowed are resources that exist only in Pulumi state: the component resources, and
  provider resources (`pulumi:providers:*`), such as an explicit provider a consumer passes in or
  the default provider of this plugin's own package.
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
- The existing deployments move onto the components first, or in parallel with the GCP work, each
  with a preview that shows no diff (Sami, 2026-10-14). This plan runs them in parallel: slice 1
  carries the existing Docker-host deployment, slice 3 the existing AWS deployment, and slice 4
  (GCP) runs beside slice 3 once slice 2's shared pieces exist.

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
  (`release-bump.sh:52` passes them to `git log` unchanged). The `envoy-and-contracts.yaml` filters
  keep it, so `envoy-smoke` still builds the listener image the live tests need.
- **R4. `host:Listener` takes an `adopt` input and an optional seed.** A consumer could alias a
  remote component's children with a transform in its own language, but `adopt` saves every
  consumer from writing one; and the existing Docker-host container has no seed.
- **R5. The watchdog supervises one listener per host, named `envoy-listener`.** Its files and
  unit names are fixed per host (`/etc/envoy/*`, `envoy-watchdog.*`), so `watchdog` with another
  `containerName` is refused. Its text substitutes the port (unchanged at 9020) and the
  component's name (the existing program echoes `machine.name`, `watchdog.ts:155`).
- **R6. The NATS authorization model and the seed command are their own slice (2),** ported from
  the existing AWS deployment's implementation with its semantics and its byte-exact output.
  Slices 3 (AWS) and 4 (GCP) both build on them and run in parallel.
- **R7. An authorization with no users and no `no_auth_user` is refused.** nats-server treats an
  empty user list as no authentication at all (v2.15.1 `server/auth.go:328-337`).
- **R8. A plugin version comes only from what the plugin compiles.** The plugin renders grants
  from Envoy's exported subject names, so a change there must cut a plugin release, and a change
  to an Envoy package it does not compile must not. The release's version step computes the bump
  paths with `go list -deps` per target, so no hand-kept list can drift from the build.
- **R9. `host:Listener` offers no listen host and no webhooks in this version.** No consumer here
  sets webhooks (the Docker-host deployment's machines all have `listener: {}`; the GCP hub has
  none in the first version, Sami 2026-10-13), and a non-loopback listener needs an API token
  (`cmd/listener/main.go:420-429`) that nothing here provides. Task 1.1 corrects the spec's list.
- **R10. A watchdog and a seed are refused together.** The watchdog recreates a removed container
  from its env file alone (`watchdog.ts`, the `missing)` branch), which holds no secret, so a
  recreated listener would come back without its seed.
- **R11. A consumer owns its secrets; a component takes references.** The AWS components take the
  listener's API token, the NATS seed, the webhook secrets and the tailnet OAuth client as secret
  ARNs. They create no `secretmanager` secret and no `random` resource. The existing AWS deployment
  declares its secret containers through a shared helper of its own, which reads each container at
  plan time, and its API token's value is copied out of band elsewhere, so it must never regenerate.
  Both stay in that repository. The same rule holds on GCP: the deployment declares the seed
  secrets, and `gcp:NatsHub` takes public keys.
- **R12. Every value of the existing AWS deployment that is not fixed by Envoy itself is an
  input,** including the base tags, metric namespaces, the rendered configuration's header lines,
  physical names and the logical name of each Cloud Map service. The component holds no
  deployment's name. An adopted child takes its logical name from the inventory and gets
  `pulumi.Alias{NoParent: pulumi.Bool(true)}` when `adopt` is set, so its URN resolves to the
  existing root-level resource.
- **R13. Container definitions are rendered byte-identically.** The AWS provider normalises the
  order and whitespace of container definitions, but compares string values byte for byte. So the
  NATS start script, the sidecar script, every health-check command and every environment value
  come out exactly as the reference implementation writes them. `core` reproduces Python's
  `json.dumps` (separators `", "` and `": "`; `sort_keys` and `ensure_ascii` where the reference
  sets them) and `shlex.quote` and `shlex.join`.
- **R14. The existing AWS deployment passes an explicit `aws` provider.** Its resources use the
  default provider today. A component's children would otherwise use the default provider of the
  plugin's own SDK version, which ties the plugin to that deployment's pin. The explicit provider is
  configured exactly as the default one is, and moving a resource between them is decided by the
  new provider's `DiffConfig` (Pulumi v3.268.0 `pkg/resource/deploy/step_generator.go:2885-2963`).
  The deployment's preview therefore also creates that provider resource, which, like the
  component resources, exists only in Pulumi state.
- **R15. Stack transformations registered with the legacy API do not reach a remote component's
  children.** The reference deployment protects no Envoy resource today. Its README says so for the
  components, so a future protection entry is written against the component's own options.

## Slices

| Slice | PR | Outcome someone can try | Depends on |
| --- | --- | --- | --- |
| 1 | sjawhar/legion#1946 | `pulumi package add envoy-infra@<v> --server '<url>'` installs a released plugin, and `envoy-infra:host:Listener` runs a healthy listener on a Docker host | none |
| 2 | a new PR stacked on #1946 (gh-stack), branch `envoy-infra-core` | `envoy-infra seed check` and `verify --store aws-secrets-manager` read an existing deployment's live seed and print the reference script's lines | Task 1.1 on `envoy-infra` |
| 3 | a new PR stacked on slice 2's (gh-stack, linear: `envoy-infra` ← `envoy-infra-core` ← `envoy-infra-aws`) | the existing AWS deployment's staging, production and dispatch previews show no diff with its NATS service and webhook listener built by `envoy-infra:aws:NatsService` and `aws:WebhookListener` | Tasks 2.1 and 2.2 |
| 4 | a plain PR based on `envoy-infra-core`, outside the gh-stack (gh-stack is strictly linear), which merges `main` once slice 2 merges and is then retargeted; branch `envoy-infra-gcp` | `envoy-infra seed generate --store gcp-secret-manager` writes a seed; `envoy-infra:gcp:NatsHub` serves NATS on GCE over a tailnet | Tasks 2.1 and 2.2 |

A stacked branch takes its lower layer's later commits by merging it (`git merge origin/<lower>`,
then a fast-forward push), never by a rebase and a force push: each PR squash-merges, so the merge
commits never reach `main`, and no published branch is rewritten. After a lower layer merges, the
branch above it merges `origin/main` the same way before its PR is retargeted. `gh stack` is used
to link the PRs, not to rebase or push them.

(The consumers' changes have their own plans in their own repositories.)

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
then in `packages/envoy/infra`: `test -z "$(gofmt -l .)"`, `go vet -tags pulumilive ./...`,
`go test ./...`. Slice 1 starts no container in this job, so `TESTCONTAINERS_RYUK_DISABLED: "true"`,
the NATS image pre-pull reading `packages/envoy/internal/testnats/nats.go`'s `const Image` and the
`natslive` vet tag arrive with slice 2, whose tests start NATS. The workflow
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
- Modify: `packages/envoy/internal/bus/reach.go` (export `LocalNATSURL`) and `nkey.go` (export
  `NkeySeedEnvVar`, the seed variable's name, which `core.EnvNATSSeed` reuses)

**Interfaces:**
- Produces:

```go
type ListenerConfig struct {
	MachineID  string
	NATSURLs   []string
	Port       int    // required: 1-65535; the component passes core.DefaultListenerPort (9020) when its input is unset
	HostBridge string // "" leaves ENVOY_HOST_BRIDGE unset
}

// ListenerEnv returns the listener's non-secret environment, KEY=value, in a fixed order.
func ListenerEnv(c ListenerConfig) ([]string, error)

const EnvNATSSeed = bus.NkeySeedEnvVar // "NATS_NKEY_SEED"; its value is a secret; the component appends it
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
	// default true; false for a listener whose NATS is not reachable from where the stack is
	// applied (the container is created and Docker restarts it until it connects)
	WaitForHealthy *bool                  `pulumi:"waitForHealthy,optional"`
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
| `<name>-container` | `docker.Container` | `Name: containerName`, `Image: image.ImageId`, `Command: ["/usr/local/bin/envoy-listener"]`, `Restart: "unless-stopped"`, `StopTimeout: 30`, `DestroyGraceSeconds: 30`, `NetworkMode: "host"`, `LogOpts: {"max-file": "5", "max-size": "20m"}`, `Envs`: `core.ListenerEnv` (computed with `ApplyT` over `natsUrls`), then `NATS_NKEY_SEED=<seed>` when `seed` is set, `Healthcheck: {Tests: ["CMD","curl","-f","http://127.0.0.1:<port>/healthz"], Interval: "10s", Timeout: "3s", Retries: 3, StartPeriod: "30s"}`, `Wait: true`, `WaitTimeout: 90` (with `waitForHealthy: false`: `Wait: false` and no `WaitTimeout`) | `DeleteBeforeReplace(true)`, alias from `adopt.container` |
| `<name>-watchdog` (only with `watchdog`) | `remote.Command` | `Connection: {Host: watchdog.host}`; `Create` and `Delete`: the Docker-host program's texts (`watchdog.ts:121-162`), byte for byte, with the env file built from the container's envs, the image reference, the port in `healthcheck_cmd` (`watchdog.ts:40`) and the component's name in the closing `echo` (`:155`) | `DependsOn(container)`, alias from `adopt.watchdog` |

Refusals, each an error naming the input: `watchdog` with a `containerName` other than
`envoy-listener` (R5); `watchdog` with `seed` (R10); a `watchdog.host` containing `@` or `://`; and
what `core.ListenerConfig.CheckPlain` refuses of the fields known at construct (a blank
`machineId`, a port outside 1-65535, a `hostBridge` with surrounding whitespace); all before any
registration. A `natsUrls` element `core.ListenerEnv` refuses fails the update, from the `ApplyT`
that computes the envs. `aws:WebhookListener` calls the same `CheckPlain` before it registers.

- [ ] **Step 1: failing unit tests** (`listener_test.go`) with `pulumi.WithMocks`, run through
`NewListener` directly:
  1. Defaults: the container's recorded inputs equal the table with `containerName =
     "envoy-listener"`, port 9020, and `Envs` as a set equal to
     `{"PORT=9020","ENVOY_MACHINE_ID=example-host-a","NATS_URLS=nats://hub.internal.example:4222","ENVOY_ALLOW_REMOTE_NATS=1","ENVOY_HOST_BRIDGE=127.0.0.1"}`
     for `MachineID: "example-host-a", NatsUrls: ["nats://hub.internal.example:4222"], HostBridge: "127.0.0.1"`.
  2. `natsUrls` given as an `Output` resolves into the same envs.
  3. A seed adds `NATS_NKEY_SEED=<seed>` and the recorded `envs` are secret.
  3a. `waitForHealthy: false` records `wait: false` and no `waitTimeout`; unset records `wait: true`
      and `waitTimeout: 90`.
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
verify it against the SHA-256 GitHub records for that release asset (written into the workflow as
a literal; the release's checksums file lists only SHA-1, which the workflow's comment cross-checks),
and set `ENVOY_INFRA_DOCKER_PLUGIN` to that tarball: the live harness installs it into
a temporary `PULUMI_HOME` of its own per test binary, and fails naming the variable when it is
unset. Then in `packages/envoy/infra`: `go test -tags pulumilive -v -timeout 15m ./host/`, and
`go build -ldflags "-X main.version=1.2.3" -o /tmp/pr ./cmd/pulumi-resource-envoy-infra && pulumi package get-schema /tmp/pr | jq -e '.version == "1.2.3"'`.
Run `.github/scripts/check-image-trigger-paths.sh` and `.github/scripts/check-action-pins.sh`.

- [ ] **Step 6: commit** `feat(envoy-infra): host:Listener`.

### Task 1.4: release

**Files:**
- Create: `.github/envoy-infra-targets.json` (multi-line, like `.github/go-release-targets.json`):
  `{"envoy-infra": ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]}`
- Create: `.github/workflows/release-envoy-infra.yaml` (the plugin's own release, on the model of `release-envoy-listener.yaml`)
- Modify: `.github/workflows/release-envoy-listener.yaml` (R3)
- Modify: `.github/workflows/pr-and-main.yaml` (notices for the new binary, cache key)

- [ ] **Step 1: release paths (R8).** The version step runs after setup-go and computes them:
`go list -deps -f '{{with .Module}}{{if .Main}}{{$.Dir}}{{end}}{{end}}' ./cmd/...` in
`packages/envoy/infra` for each target, keeping only directories under `"$PWD"/packages/envoy/`
(a `go.work` that binds a module elsewhere fails the step), plus each main module's `go.mod` and
`go.sum`, `GOWORK` and its `.sum`, and, as patch paths, `scripts/go-third-party-notices.sh` and
`.github/envoy-infra-targets.json`, which decide what the archives hold. A `go list` failure or an
empty target list fails the step with no output. The workflow's push paths are
`packages/envoy/**` plus every input outside it the build reads (the notices script, the targets
file, `.github/actions/install-pulumi/**`, the workflow), so a push that changes no compiled
package runs the workflow and skips. The required `lint` job compiles every command for every
target, so a platform-only break fails the pull request.

- [ ] **Step 2: the workflow.** `release-envoy-infra.yaml`: `on.push` to `main` on the push paths
of Step 1, plus `workflow_dispatch`; `permissions: {}` at the top; workflow-level
`concurrency: {group: release-envoy-infra-${{ github.ref }}, cancel-in-progress: false}`, so one
run's publish always tags before the next run's build versions; waiting on no other release. Two
jobs; only `build` carries `if: github.ref == 'refs/heads/main'`, and `publish` runs only after it:
- `build` (`contents: read`, checkout with full history and `persist-credentials: false`): the
  version step, `go vet -tags pulumilive ./...` and `go test ./...` in the module, the per-target
  build with `-trimpath`, the notices, the smoke test, then `upload-artifact` of the archives and
  the version as a job output. It runs the module's code and every module it links, so it holds no
  write scope, no secret and no OIDC token.
- `publish` (`needs: build`; `contents: write`, `id-token: write`, `attestations: write`): runs no
  Go, Pulumi or plugin code. It refuses a `VERSION` that isn't plain X.Y.Z (`is_plain_version`),
  derives the tag itself as `envoy-infra-v$VERSION`, and through `release-push.sh handed` requires
  exactly the expected archives, as regular files (one per target in its own checkout of the
  targets file, and per archive kind). It then writes `SHA256SUMS` over that list itself, so
  the sums are publish's own record of the bytes it was handed. It pushes the tag with the deploy
  key through `release-push.sh push-tag`, attests the archives only after the tag is pushed (a
  refused tag leaves no attestation), and releases. Build chooses the bytes, never the names or
  their sums.

The version step:

```bash
PREV_TAG=$(git tag -l 'envoy-infra-v*' --sort=-v:refname | head -1)
if [ -z "$PREV_TAG" ]; then PREV_VERSION=0.0.0; RANGE=HEAD; else PREV_VERSION="${PREV_TAG#envoy-infra-v}"; RANGE="${PREV_TAG}..HEAD"; fi
VERSION=$(.github/scripts/release-bump.sh "$PREV_VERSION" "$RANGE" \
  --patch-path scripts/go-third-party-notices.sh --patch-path .github/envoy-infra-targets.json \
  -- <the paths of Step 1>)
```

With no `envoy-infra-v*` tag the first release is seeded at `0.1.0`, as `release-envoy-listener.yaml`
seeds its first tag: the breaking commits in the paths' earlier history predate the plugin and say
nothing about its API. From the second release on, the formula above runs over `PREV_TAG..HEAD`.

On `empty` or `none` it skips with the same `::notice` and step-summary line the `legion` job
writes (`:286-297`). Build per target with `CGO_ENABLED=0`, `-trimpath` and
`-ldflags "-X main.version=$VERSION"`, write `THIRD_PARTY_NOTICES` with
`scripts/go-third-party-notices.sh`, pack `pulumi-resource-envoy-infra-v$VERSION-$os-$arch.tar.gz`
with the binary and notices at the archive root. Smoke the linux/amd64 binary with the pinned
Pulumi CLI:
`pulumi package get-schema <binary> | jq -e --arg v "$VERSION" '.version == $v and .pluginDownloadURL == "https://github.com/sjawhar/legion/releases/download/envoy-infra-v${VERSION}"'`.
In `publish`, once the tag is pushed, attest provenance with `actions/attest-build-provenance` on
exactly the archives (consumers run `gh attestation verify` on them), then release through
`release-push.sh release`, as the other release workflows do.

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

- [ ] Once #1946 is merged and `release-envoy-infra.yaml` has run: record the run id and the
  `envoy-infra-v<v>` tag; from an empty directory, `pulumi package add envoy-infra@<v> --server 'https://github.com/sjawhar/legion/releases/download/envoy-infra-v${VERSION}'`
  succeeds, and `pulumi preview` of a YAML program with one explicit `pulumi:providers:docker` and
  one `envoy-infra:host:Listener` plans creates of exactly the stack, that provider, the component,
  its `docker:index/remoteImage:RemoteImage` and its `docker:index/container:Container`. Record the
  commands and output on #1946.

---

## Slice 2: NATS authorization and the seed command, from the existing AWS deployment

Branch `envoy-infra-core` in its own clone (`~/.worktrees/legion/envoy-infra-core`), stacked on
`envoy-infra` with gh-stack. Task 2.1 starts once Task 1.1 is on `envoy-infra`; 2.2 needs 2.1; 2.3
needs 2.2 and Task 1.4.

The reference implementation is the existing AWS deployment's Envoy code. Its implementer gets the
inventory of that code with the brief, from outside this repository. Nothing private about it is
written here.

### Task 2.1: NATS authorization (`core`)

**Files:**
- Create: `packages/envoy/infra/core/nats_auth.go`, `nats_auth_test.go`, `pyfmt.go`,
  `pyfmt_test.go`, `testdata/authorization.golden`, `testdata/start-script.golden`
- Modify: `packages/envoy/internal/testnats/nats.go`:
  - (`startNkeyConfig` stays unexported: Task 2.2's live test uses `StartImageConfig`);
  - add `const HubImage = "mirror.gcr.io/library/nats:2.15.1-alpine@sha256:2ae57dfba3063d5e25fecb75256118c0882a1a2d2cf7d0fba17a7c701df956f6"`
    on one line beside `Image`;
  - add `StartImageConfig(t, image, config string, serverArgs []string, anonymous bool)`. It runs
    `image` with the config mounted at `/etc/nats/nats-server.conf` and the arguments
    `-c /etc/nats/nats-server.conf` followed by `serverArgs`. It waits with the existing `answering`
    check when `anonymous` is false, and for a credential-less connect to succeed when it is true.
    Task 2.1's tests pass `-js`; Task 4.1's pass the hub's own `-js --store_dir /data -m 8222`.
- Modify: `.github/workflows/envoy-and-contracts.yaml`: `envoy-infra-go`'s pre-pull also pulls
  `const HubImage`.

**Interfaces:**

```go
type User struct {
	Name      string // ^[a-z0-9](?:[a-z0-9-]{0,46}[a-z0-9])?$
	NKey      string // nkey user public key; "" for a pending user, which is never rendered
	Publish   *Rules // required with NKey, refused without
	Subscribe *Rules // required with NKey, refused without
	HasSeed   bool   // the consumer stores a seed for this user
}

type Rules struct {
	Allow []string // must name at least one entry: nats-server reads an empty allow as "everything"
	Deny  []string
}

type Authorization struct {
	Users      []User
	NoAuthUser string // "" for none; else the Name of a keyed user without a seed
}

func (a Authorization) Validate() error
// Render returns the authorization block: the header lines, each as "# <line>", then the block.
func (a Authorization) Render(header []string) (string, error)
// StartScript returns the `sh -c` script that writes Render's text to path and execs nats-server.
func (a Authorization) StartScript(header []string, serverArgs []string, path string) (string, error)
```

These semantics are ported from the reference implementation, and the unit tests pin each one:
- **What is refused.** Every refusal names the user and the field.
  - No users.
  - Every user pending. That would render a block with no users, which is R7's case.
  - A name that fails the pattern.
  - A pending user that declares rules.
  - A keyed user missing either rule.
  - An NKey that is not a user public key. The check is `U` plus 55 base32 characters with a valid
    CRC-16/XMODEM, which is `nkeys.IsValidPublicUserKey`. A pasted seed fails here too.
  - An empty allow list.
  - An entry that is neither a subject pattern nor `<subject> <queue>`. A subject pattern is made
    of dot-separated non-empty tokens, with `*` and `>` only as whole tokens and `>` only last, and
    with no whitespace, control or surrogate character (Unicode categories Cc and Cs, and
    `unicode.IsSpace` plus U+001C-U+001F).
  - A queue-scoped entry on the publish side.
  - Duplicate names, or duplicate keys.
  - A `NoAuthUser` that names no user, names a pending user, or names a user with `HasSeed`.
- **How the block is rendered.** The lines are, in order:
  - each header line as `# <line>`;
  - `authorization {`;
  - `  users: [`;
  - per keyed user, in input order: `    # <name>`, then four spaces and the JSON object
    `{"nkey": …, "permissions": {"publish": {"allow": […], "deny": […]}, "subscribe": {…}}}`.
    Keys are sorted, the separators are `", "` and `": "`, non-ASCII stays raw, and `deny` is
    omitted when empty;
  - `  ]`;
  - `}`;
  - with a `NoAuthUser`: `# <name>: a client presenting no credential becomes this user`, then
    `no_auth_user: "<key>"`, the key JSON-quoted.

  The text ends with one newline.
- **How the start script is built.**
  `set -eu; printf '%s' <q(text)> > <q(path)>; exec <join(["nats-server", "--config", path, …serverArgs])>`.
  Here `q` is Python's `shlex.quote`: it returns `''` for an empty string, the value unchanged when
  every character is in `[A-Za-z0-9_@%+=:,./-]`, and otherwise the value wrapped in `'` with each
  `'` written as `'"'"'`. `join` is `q` over each argument, joined by single spaces.
- `pyfmt.go` holds `PyJSON(v any, opts PyJSONOptions) string` and `ShellQuote` and `ShellJoin`,
  for Task 3.x's container definitions too. `PyJSONOptions` has `SortKeys` and `EnsureASCII`. With
  `EnsureASCII`, every non-ASCII character is written as `\uXXXX`, using surrogate pairs above
  U+FFFF, as Python does.

- [ ] **Step 1: goldens from the reference.** The reference's renderer writes its own two header
  lines, and they name its repository's module and a private issue key. So the goldens are made
  in three steps:
  1. Run the reference's renderer on the placeholder authorization in this task's brief: three
     keyed users `example-listener`, `example-daemon` and `example-fallback` (a `NoAuthUser` must
     name a keyed user without a seed), one pending user `example-pending`, `NoAuthUser`
     `example-fallback`, a subject with a non-ASCII character, and a queue-scoped subscribe entry.
  2. Replace exactly its first two lines with `# Rendered by envoy-infra from placeholder inputs.`
     and `# Public keys only.` (the header `core` is given in the test). Check that nothing else
     differs from the reference's output, and commit the result as the config golden.
  3. Build the start-script golden from that config text with the reference's own expression,
     unchanged: `"set -eu; printf '%s' " + shlex.quote(text) + " > " + shlex.quote(path) + "; exec " + shlex.join(["nats-server", "--config", path, *args])`,
     with the server args `["-js", "--store_dir", "/data", "-m", "8222"]` and the path
     `/tmp/nats-server.conf`.

  Run the script that does this in the deployment repository's clone, and do not commit it. The
  commit message says that the goldens were rendered by the reference implementation from
  placeholder inputs, with the header lines substituted, and names neither the deployment nor
  its repository.
- [ ] **Step 2: failing tests.**
  - A validation table with one row per refusal.
  - The goldens, matched byte for byte.
  - `PyJSON` against fixed Python outputs for: nested maps with `SortKeys`; a string holding `"`,
    `\`, `<`, `>`, `&`, U+00E9, U+2028 and U+1F600, both with and without `EnsureASCII`; and an
    empty list.
  - `ShellQuote` for `""`, `a-b_c`, `a b`, `it's` and a string with a newline.
- [ ] **Step 3: failing real-server tests.** Run each one twice: on `testnats.Image` (2.10) and on
  `testnats.HubImage`, both through `StartImageConfig`. Each server is given the rendered block as
  its whole configuration.
  - A client authenticating with user A's seed (`nkeys.CreateUser`) connects.
  - With no `NoAuthUser`, a client with no credential is refused.
  - User A publishing outside its allow list gets a permissions violation.
  - With `NoAuthUser` (`anonymous: true`), a credential-less client connects with that user's
    permissions.
  - The start script, run by `/bin/sh` against a temporary path, writes exactly Render's text.
- [ ] **Step 4: implement.** Run `cd packages/envoy && go test ./internal/testnats/` and
  `cd packages/envoy/infra && go test ./core/`. Commit
  `feat(envoy-infra): NATS authorization, byte-compatible with the reference renderer`.

### Task 2.2: `envoy-infra seed` with the AWS Secrets Manager store

**Files:** Create `packages/envoy/infra/cmd/envoy-infra/main.go`, `seed/seed.go`, `seed_test.go`,
`seed/awsstore.go`, `awsstore_test.go`, `seed/seed_live_test.go` (build tag `natslive`). Modify
`.github/workflows/envoy-and-contracts.yaml`: `envoy-infra-go` runs `go test -tags natslive ./core/ ./seed/`.

**Interfaces:**

```go
type Store interface {
	// Describe reports whether the secret exists, whether it is scheduled for deletion, and the
	// version it serves by default, with the store's one metadata read (DescribeSecret, GetSecret),
	// so check and verify need no grant the reference did not.
	Describe(ctx context.Context, name string) (Description, error)
	// Versions counts every version the secret holds, deprecated ones included; generate alone
	// calls it.
	Versions(ctx context.Context, name string) (int, error)
	// Put writes value as the secret's first version and returns its version id.
	Put(ctx context.Context, name string, value []byte) (version string, err error)
	// Current reads the version the store serves by default.
	Current(ctx context.Context, name string) (version string, value []byte, err error)
}

type Description struct {
	Exists, ScheduledForDeletion bool
	DeletionDate                 string
	CurrentVersion               string // "" when the store serves none
	// Task 4.2 adds NewestVersion and NewestState, for a store whose default names the newest
	// version whatever its state (GCP's versions/latest).
}

func Check(ctx context.Context, s Store, name string) (string, error) // the report line
func Generate(ctx context.Context, s Store, name string) (publicKey, version string, err error)
func Verify(ctx context.Context, s Store, name, publicKey string) (line string, err error) // the report line in every case

var ErrRefused = errors.New("seed: refused") // nothing was written: exit 2
// A Verify mismatch, and any error after Put returned, exit 1.
```

The CLI is `envoy-infra seed {check|generate|verify} --store aws-secrets-manager --name <secret id>
[--region <r>] [--expect-account <id>] [--public-key U…]`. Its behaviour is the reference
implementation's:
- **The account check.** `--expect-account` refuses with exit 2 unless
  `sts:GetCallerIdentity` returns that account.
- **`generate`.** It refuses unless the container exists, is not scheduled for deletion, and holds
  no version. It writes the bare `SU…` seed string with `PutSecretValue` and a fresh
  `ClientRequestToken`. It requires `AWSCURRENT` among the stages returned, then re-lists the
  versions and reads the value back to confirm that it derives the printed key. It prints
  `public key: U…` and `version id: …`.
- **`check`.** It prints exactly one of the reference script's three lines: `<name>: absent`,
  `<name>: present, scheduled for deletion (<date>)`, or
  `<name>: present; AWSCURRENT: yes (version <id>)|no`.
- **`verify`.** It compares the key derived from the `AWSCURRENT` value with `--public-key`, and
  prints the reference's line: `<name> AWSCURRENT (version <id>) derives <key>: yes`, exit 0, or
  `…: no`, exit 1.
- **Secrecy.** The seed never reaches argv, stdout, stderr, a log or an error message, and the AWS
  SDK's logging is off.

The store uses aws-sdk-go-v2 (`secretsmanager`, `sts`). Pin the latest release when the task
starts, and record the version.

- [ ] **Step 1: failing tests** against a fake `Store`:
  - every refusal exits 2 and writes nothing;
  - `generate` reads back the version it wrote;
  - a canary seed appears in no output or error;
  - `verify` passes for the right key and exits 1 naming the secret for another;
  - `check`'s three lines.
- [ ] **Step 2: the AWS store,** tested against an `httptest` server that speaks the Secrets Manager
  JSON protocol for `DescribeSecret`, `ListSecretVersionIds` (with `IncludeDeprecated`),
  `PutSecretValue` and `GetSecretValue`, and the STS query protocol for `GetCallerIdentity`. The
  SDK reaches it through `BaseEndpoint`.
- [ ] **Step 3: the seed reaches NATS** (`seed_live_test.go`). `Generate` against the fake store;
  render a `core` authorization with the returned key; start the server with
  `testnats.StartImageConfig` on both images. A client using the stored seed connects, and one
  without it is refused.
- [ ] **Step 4: commit** `feat(envoy-infra): seed command with the AWS Secrets Manager store`.

### Task 2.3: release the CLI, and the slice's proof

- [ ] **Extend `release-envoy-infra.yaml`.** Also build `./cmd/envoy-infra` per target into
  `envoy-infra-$os-$arch.tar.gz`. Each archive holds one directory, `envoy-infra-$os-$arch/`, with
  `envoy-infra` and `THIRD_PARTY_NOTICES`: the `legion` layout, so a mise `github:` pin works. Add
  the notices loop for it in `pr-and-main.yaml`. Widen the attestation's `subject-path` to cover
  `assets/envoy-infra-*.tar.gz` too: the CLI writes seeds under its operator's credentials.
- [ ] **README.** Document the seed workflow: the consumer declares an empty secret,
  `envoy-infra seed generate` writes it, and the public key goes into the authorization. Also
  document `check` and `verify`, the exit codes, and `core.Authorization`'s rules, and tell an
  operator to run `gh attestation verify <archive> --repo sjawhar/legion --cert-identity https://github.com/sjawhar/legion/.github/workflows/release-envoy-infra.yaml@refs/heads/main --source-ref refs/heads/main --deny-self-hosted-runners`
  before installing the CLI (`--repo` alone accepts an attestation from any workflow run in the
  repository; docs/kubernetes.md pins the same three flags).
  Commit `docs(envoy-infra): NATS authorization and seeds`.
- [ ] **Pre-merge proof (E10).** On the devbox, run the branch's CLI read-only against the existing
  deployment's one live seed: `envoy-infra seed check` and
  `verify --store aws-secrets-manager --expect-account <its account>` with that seed's recorded
  public key. The secret id, account and key come from that deployment's stack configuration, given
  in the brief. Both must answer as the reference script does today. Whether `core` renders that
  deployment's authorizations byte for byte is shown by E11: its task definitions must not change.
  The record goes on the deployment repository's PR. This PR gets outcomes and durations only, with
  placeholders.

---

## Slice 3: the existing AWS deployment's components

Branch `envoy-infra-aws` in its own clone (`~/.worktrees/legion/envoy-infra-aws`), stacked on
`envoy-infra-core`. Task 3.1 owns the pulumi-aws dependency (`go.mod`, `go.sum`); 3.2 needs 3.1,
and its empty-list golden needs the deployment plan's PR 0 commit (Task 3.4);
3.3 needs 3.2.

Every task in this slice takes the reference inventory, supplied with its brief: each resource's
type, logical name, inputs, physical name, tags and options, the exact container definitions, and
what the deployment's other code reads from them. The components reproduce that inventory exactly,
under R11-R15. The args structs below are the whole interface. Every field that holds a deployment
value is required and has no default, and the brief gives the deployment's value for each one.

### Task 3.1: `envoy-infra:aws:NatsService`

**Files:** Create `packages/envoy/infra/aws/natsservice.go`, `natsservice_test.go`,
`containers.go`, `containers_test.go`, and `testdata/nats-*.golden`. Modify `provider.go`
(register), `go.mod` and `go.sum` (pulumi-aws Go SDK v7, the latest release when the task starts).

**Interfaces:**

```go
type NatsServiceArgs struct {
	// The authorization, as core.Authorization takes it; nil runs NATS with no authorization
	// block, as the reference's launch does when a stack declares none.
	Authorization *AuthorizationArgs `pulumi:"authorization,optional"`
	ConfigHeader  []string           `pulumi:"configHeader"`   // the rendered config's header lines
	ServerArgs    []string           `pulumi:"serverArgs"`
	ConfigPath    string             `pulumi:"configPath"`     // where the task's shell writes the config

	NatsImage           string `pulumi:"natsImage"`
	TailscaleImage         string             `pulumi:"tailscaleImage"`
	TailnetHostname        string             `pulumi:"tailnetHostname"`
	TailnetTags            []string           `pulumi:"tailnetTags"`              // joined with "," into --advertise-tags
	TailnetClientSecretArn pulumi.StringInput `pulumi:"tailnetClientSecretArn"`   // the sidecar's OAuth client secret
	TailnetEphemeral       bool               `pulumi:"tailnetEphemeral,optional"` // the auth key's ?ephemeral= value; false unless set

	// Values a consumer feeds from other resources are Inputs: a remote component's construct
	// refuses a plain field whose input carries a dependency (Pulumi SDK v3.265.0
	// go/pulumi/provider.go:765-766), and infer marks a plain field `plain` in the schema.
	Region           string               `pulumi:"region"`
	VpcId            pulumi.StringInput   `pulumi:"vpcId"`
	SubnetIds        []pulumi.StringInput `pulumi:"subnetIds"`        // a slice, so the component knows how many mount targets to register
	SecurityGroupId  pulumi.StringInput   `pulumi:"securityGroupId"`  // the NATS task's group, built by the consumer
	ClusterArn       pulumi.StringInput   `pulumi:"clusterArn"`
	ExecutionRoleArn pulumi.StringInput   `pulumi:"executionRoleArn"`
	TaskRoleArn      pulumi.StringInput   `pulumi:"taskRoleArn"`

	CloudMapNamespaceId        pulumi.StringInput `pulumi:"cloudMapNamespaceId"`
	CloudMapNamespaceName      pulumi.StringInput `pulumi:"cloudMapNamespaceName"`
	CloudMapServiceName        string             `pulumi:"cloudMapServiceName"`        // the DNS name
	CloudMapServiceLogicalName string             `pulumi:"cloudMapServiceLogicalName"` // the child's Pulumi name

	Family           string `pulumi:"family"`
	LogGroupName     string `pulumi:"logGroupName"`
	LogRetentionDays int    `pulumi:"logRetentionDays"`
	Cpu              string `pulumi:"cpu"`
	Memory           string `pulumi:"memory"`
	PlatformVersion  string `pulumi:"platformVersion"`

	BaseTags           map[string]string `pulumi:"baseTags"`   // the component adds `service` per child
	MetricNamespace    string            `pulumi:"metricNamespace"`
	WaitForSteadyState *bool             `pulumi:"waitForSteadyState,optional"` // unset leaves the input unset
	Adopt              bool              `pulumi:"adopt,optional"`
}

type NatsService struct {
	pulumi.ResourceState
	ServiceArn        pulumi.StringOutput    `pulumi:"serviceArn"`
	ServiceName       pulumi.StringOutput    `pulumi:"serviceName"`
	EfsId             pulumi.StringOutput    `pulumi:"efsId"`
	LogGroupName      pulumi.StringOutput    `pulumi:"logGroupName"`
	TaskDefinitionArn pulumi.StringOutput    `pulumi:"taskDefinitionArn"`
	InternalUrl       pulumi.StringOutput    `pulumi:"internalUrl"`     // nats://<cloud map name>.<namespace>:4222
	MetricNamespace   pulumi.StringOutput    `pulumi:"metricNamespace"`
}
```

`AuthorizationArgs` mirrors `core.Authorization` field for field (`users`: `name`, `nkey`,
`publish`, `subscribe`, `hasSeed`; `noAuthUser`).

- [ ] **Resources, as children.**
  - Two EFS file systems, each with its security group, rules, one mount target per subnet and an
    access point. One holds JetStream's data, the other the sidecar's tailnet state.
  - The log group.
  - The Fargate task definition, with the NATS container and the Tailscale sidecar.
  - The Cloud Map service and the ECS service.
  - The four log metric filters.

  Each child is registered under the logical name the inventory gives it, `cloudMapServiceLogicalName`
  for the Cloud Map service. The ECS service's `dependsOn` covers every mount target.
  `waitForSteadyState` reaches the service only when set.
- [ ] **Container definitions.** Build them with `core.PyJSON`, no `SortKeys`, `EnsureASCII` on, in the
  reference's dict order. `PyJSON` refuses a Go map without `SortKeys` (map order is random), so
  this task adds `core.Object`, an ordered list of key/value pairs that `PyJSON` writes in order, with
  its own `pyfmt_test.go` cases against fixed Python outputs. The NATS launch is `{"entryPoint": ["/bin/sh", "-c"], "command":
  [<StartScript>]}` when `authorization` is set, and the reference's plain launch when it is not.
  The sidecar's script is exactly as in the inventory. Goldens are rendered by the reference
  implementation from placeholder inputs. Its start script embeds the config text, whose first two
  lines name its repository's module and a private issue key, inside the container JSON, so the
  scratch script replaces the reference module's `render_server_config` with a wrapper that
  substitutes exactly those two lines (Task 2.1's header lines) and then runs the reference's own
  container-definition code unchanged. The check: neither original header line appears in any
  golden, and a second run without the wrapper differs from the golden only in those two lines'
  text.
- [ ] **Tests.** With `pulumi.WithMocks`, cover:
  - every child's type, logical name, recorded inputs and parent;
  - the alias when `adopt` is set, and none without it;
  - refusal without an `aws` provider, before any registration;
  - the component through the provider's real `Construct` (`integration.NewServer(…,
    integration.WithMocks(…)).Construct`, pulumi-go-provider v1.7.0 `integration/integration.go`),
    with every Input field sent as an output value carrying a dependency URN, and again with
    those values unknown, as under preview: both construct, and the recorded children match the
    mock test's; and in a preview whose inputs are known, every component output is known (a
    stack output the consumer builds from one would otherwise show as changed);
  - `waitForSteadyState` unset leaves the service's input unset;
  - the goldens, byte for byte.
- [ ] **Commit** `feat(envoy-infra): aws:NatsService`.

### Task 3.2: `envoy-infra:aws:WebhookListener`

**Files:** Create `packages/envoy/infra/aws/webhooklistener.go`, `webhooklistener_test.go` and
`testdata/listener-*.golden`; modify `provider.go`.

**Interfaces:**

```go
type WebhookListenerArgs struct {
	MachineId string                  `pulumi:"machineId"`
	NatsUrls  pulumi.StringArrayInput `pulumi:"natsUrls"`
	Image     string                  `pulumi:"image"`
	Region    string                  `pulumi:"region"` // the container's awslogs-region

	ApiTokenSecretArn       pulumi.StringInput    `pulumi:"apiTokenSecretArn"`
	ApiTokenSecretVersionId pulumi.StringInput    `pulumi:"apiTokenSecretVersionId"` // orders the task definition after the token's version
	SeedSecretArn           pulumi.StringPtrInput `pulumi:"seedSecretArn,optional"`
	Webhooks                []string              `pulumi:"webhooks"` // the ENVOY_WEBHOOKS list, e.g. ["github", "slack"]
	GitHubWebhookSecretArn  pulumi.StringPtrInput `pulumi:"githubWebhookSecretArn,optional"`
	SlackSigningSecretArn   pulumi.StringPtrInput `pulumi:"slackSigningSecretArn,optional"`
	ReviewerAppId           *string               `pulumi:"reviewerAppId,optional"`

	ClusterArn        pulumi.StringInput   `pulumi:"clusterArn"`
	ExecutionRoleArn  pulumi.StringInput   `pulumi:"executionRoleArn"`
	ExecutionRoleName pulumi.StringInput   `pulumi:"executionRoleName"` // the role policies' `role`
	TaskRoleArn       pulumi.StringInput   `pulumi:"taskRoleArn"`
	SubnetIds         []pulumi.StringInput `pulumi:"subnetIds"`
	SecurityGroupId   pulumi.StringInput   `pulumi:"securityGroupId"`
	TargetGroupArn    pulumi.StringInput   `pulumi:"targetGroupArn"`

	CloudMapNamespaceId        pulumi.StringInput `pulumi:"cloudMapNamespaceId"`
	CloudMapNamespaceName      pulumi.StringInput `pulumi:"cloudMapNamespaceName"`
	CloudMapServiceName        string             `pulumi:"cloudMapServiceName"`
	CloudMapServiceLogicalName string             `pulumi:"cloudMapServiceLogicalName"`

	Family           string `pulumi:"family"`
	LogGroupName     string `pulumi:"logGroupName"`
	LogRetentionDays int    `pulumi:"logRetentionDays"`
	Cpu              string `pulumi:"cpu"`
	Memory           string `pulumi:"memory"`

	// The listener's remaining environment, every one a value of its deployment (R12).
	ListenHost     string `pulumi:"listenHost"`     // ENVOY_LISTEN_HOST
	HostBridge     string `pulumi:"hostBridge"`     // ENVOY_HOST_BRIDGE
	NatsReplicas   int    `pulumi:"natsReplicas"`   // ENVOY_NATS_REPLICAS
	MentionTrigger *string `pulumi:"mentionTrigger,optional"` // ENVOY_GITHUB_MENTION_TRIGGER; required with github

	AlarmTopicArn   pulumi.StringInput `pulumi:"alarmTopicArn"`
	BaseTags        map[string]string  `pulumi:"baseTags"`
	MetricNamespace string             `pulumi:"metricNamespace"`
	NatsServiceArn  pulumi.StringInput `pulumi:"natsServiceArn"` // NatsService's serviceArn; orders this service after it
	Adopt           bool               `pulumi:"adopt,optional"`
}

type WebhookListener struct {
	pulumi.ResourceState
	ServiceArn        pulumi.StringOutput `pulumi:"serviceArn"`
	ServiceName       pulumi.StringOutput `pulumi:"serviceName"`
	LogGroupName      pulumi.StringOutput `pulumi:"logGroupName"`
	TaskDefinitionArn pulumi.StringOutput `pulumi:"taskDefinitionArn"`
	InternalUrl       pulumi.StringOutput `pulumi:"internalUrl"` // http://<cloud map name>.<namespace>:9020
}
```

- [ ] **Resources, as children.**
  - The log group and the Cloud Map service.
  - The execution-role policies that read the API token and, when it is given, the seed.
  - The task definition. It depends on `apiTokenSecretVersionId` and on those policies.
  - The ECS service. It has a load-balancer attachment on the target group with container port
    9020, a circuit breaker, a health-check grace, and custom timeouts of 15 minutes on create and
    update. It depends on `natsServiceArn`.
  - The eight log metric filters and the five alarms, with the pattern and description text each
    one has in the inventory. That text names no deployment: confirm this as you port it, and stop
    and report if it does.

  Ordering inputs go through `pulumi.DependsOnInputs`. Each of these is refused before any
  registration, naming the field and its webhook (a nil `StringPtrInput` is visible at
  construct):
  - a webhook listed in `webhooks` without its secret ARN;
  - `github` listed without `reviewerAppId` or `mentionTrigger`;
  - a secret ARN, `reviewerAppId` or `mentionTrigger` given for a webhook that is not listed,
    which would otherwise be dropped silently.

  Each refusal has its own test row.
- [ ] **Container definitions, policies and environment.** Build them inside
  `pulumi.All(…).ApplyT` over the Input fields, as the reference builds them inside
  `Output.all(...).apply`. Each environment value comes from `core.ListenerEnv` where the variables
  match, and from the fields above for the rest. `ENVOY_WEBHOOKS` is written only when `webhooks`
  is non-empty, `ENVOY_GITHUB_MENTION_TRIGGER` and `ENVOY_REVIEWER_APP_ID` only when `github` is
  listed, and each webhook's secret entry only when its webhook is listed. That is the reference's
  rule once the deployment makes its listener slot-capable; for a deployment that lists both, the
  container is today's. The goldens show the result is byte for byte: one for both webhooks,
  rendered by the reference implementation. The first version accepts only the full list, refusing
  any other naming it; Task 3.4 adds the empty list once the deployment plan's PR 0 (its Task 0)
  renders it.
- [ ] **Tests,** as in Task 3.1, including its construct test.
- [ ] **Commit** `feat(envoy-infra): aws:WebhookListener`.

### Task 3.3: `envoy-infra render`, documentation and the slice's proof

The deployment runs two gates before every listener image pin moves:
- a grant rig, which runs a real NATS and a real listener under the production grant;
- a checker, which runs the listener's log-metric-filter patterns against real log lines through
  CloudWatch.

Both read the rendered launch, environment and patterns from its Python components. Once those
are gone, they read them from this command.

- [ ] **`envoy-infra render`** (`cmd/envoy-infra`). Each subcommand prints to stdout and returns
  exactly what the components register:
  - `render nats-start-script --authorization-file <json> --config-header <line>... --server-arg <arg>... --config-path <path>`
    prints `Authorization.StartScript`.
  - `render webhook-listener-env --machine-id <id> --nats-url <url>... [--webhook <name>]... [--reviewer-app-id <id>] [--mention-trigger <t>] --listen-host <h> --host-bridge <h> --nats-replicas <n>`
    prints the container's non-secret `KEY=value` lines in order. It applies the refusals that its
    own flags can meet, each with its own test row: `--webhook github` without `--reviewer-app-id`
    or `--mention-trigger`; either of those flags without `--webhook github`; and any list other
    than the full one (`github`, `slack`) or the empty one.
  - `render metric-filters --component nats-service|webhook-listener --metric-namespace <ns> --logical-name-prefix <p> --json`
    prints each filter's logical name, pattern, metric name and namespace; `<p>` is the component's
    `metricFilterLogicalNamePrefix` (Task 3.1 added the input: the reference names its filters with a
    literal prefix, not from the component's own name).

  A test pins each subcommand's output against the component's recorded inputs under mocks for the
  same arguments. That keeps one source.
- [ ] **README.** Document:
  - `aws:NatsService` and `aws:WebhookListener`'s inputs, with a Python example using placeholders;
  - R11-R15, written for an operator;
  - how an existing deployment moves onto them: `adopt`, an explicit provider configured like the
    default, and the preview check;
  - `envoy-infra render`.

  Commit `docs(envoy-infra): AWS components`.
- [ ] **Pre-merge proof (E11).** The existing AWS deployment's own plan runs its staging,
  production and dispatch previews.
  - They must create, update, replace or delete no cloud resource, and stack outputs must not
    change. The only creates allowed are those the Global constraints name.
  - The previews use this branch's plugin, built by path, and run from the devbox: staging under
    its `staging` profile, production and dispatch under the instance identity.
  - The record goes on that repository's draft PR, opened early for it. This PR gets operation
    counts by resource type only.

### Task 3.4: the empty webhooks list

Sami left the deployment's proof surface to the coordinator on 2026-10-14, which chose a
slot-capable Envoy (the deployment plan's PR 0): a standalone slot runs a listener with no webhooks.
Once that PR has a commit, the coordinator supplies it in the brief:

- [x] Render the empty-list container definition with that commit's reference code from placeholder
  inputs (header lines substituted as in Task 2.1), and commit it as the second golden.
- [x] `aws:WebhookListener` and `render webhook-listener-env` accept `webhooks: []` as well as the
  full list, and still refuse any other list, naming it. The component's output for `[]` matches the
  golden byte for byte; a refusal row per partial list stays.
- [x] The README's webhooks row says so.
- [x] Commit `feat(envoy-infra): a webhook listener with no webhooks` (`86373960`, from the reference
  at the deployment's PR 0 commit `0b6a25ed`).

---

## Slice 4: `gcp:NatsHub`

Branch `envoy-infra-gcp` in its own clone (`~/.worktrees/legion/envoy-infra-gcp`), a plain PR based
on `envoy-infra-core` outside the gh-stack, which merges `main` once slice 2 merges. Task
4.1 starts once Task 2.1 is on `envoy-infra-core`; 4.2 needs Task 2.2; 4.3 needs 4.1; 4.4 needs 4.2
and 4.3.

### Task 4.1: the listener's and a session's permission sets (`core`)

**Files:**
- Create: `packages/envoy/infra/core/permissions.go`, `permissions_test.go`,
  `core/permissions_live_test.go` (build tag `natslive`)
- Modify: `packages/envoy/internal/bus` (exported `ListenerDurable(machineID string) string` and
  `ListenerQueue(machineID string) string` for `"listener-" + machineID` with spaces as `-`
  (`cmd/listener/main.go:531`) and `"envoy-listener-" + machineID` (`:634`)), `cmd/listener/main.go`
  (calls them)

**Interfaces:**

```go
func ListenerPermissions(machineID string) Permissions // the grant envoy-listener needs
func SessionPermissions() Permissions                  // the grant any agent session needs
func HostPermissions(machineID string) Permissions     // their union: one NATS user per host
```

Built from Envoy's exported names: `bus.Stream`, `bus.StreamSubjects()`, `bus.ListenerDurable`,
`bus.ListenerQueue`, the KV bucket names `bus.InterestBucket`, `bus.RoleClaimBucket`,
`bus.SessionBucket` and `bus.CIStateBucket` (moved into `bus` from `store`, `session` and
`cistore`, so `core` compiles in no Envoy package beyond `bus` and `contracts` and a change to those
stores cuts no plugin release), `contracts.AgentStreamFramesSubject` and `AgentStreamControlSubject`
with `*` for the session id.
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
- [ ] **Step 2: failing live test** (`natslive`), on both images as in Task 2.1: one user holding
  `HostPermissions("example-host-a")`; build and run `packages/envoy/cmd/listener` with its seed
  (`NATS_NKEY_SEED`), `ENVOY_MACHINE_ID=example-host-a` and the server's loopback URL; require
  `/healthz` to answer `"status":"healthy"` within 60 s (the durable bound under the grant); a
  session client with the same seed subscribes to `notifications.agent.s1` and publishes to
  `agentstream.s1.frames` with no permissions error; a message `POST`ed for `s1` to the listener's
  `/v1` API reaches the session. Negative control: the same run with `$JS.API.CONSUMER.CREATE…`
  removed from the grant never reaches "healthy" and the server logs a permissions violation. The
  `natslive` run Task 2.2 added to `envoy-infra-go` (`./core/ ./seed/`) already picks up this file.
- [ ] **Step 3: implement**, pass, commit `feat(envoy-infra): permission sets from Envoy's own subjects`.

### Task 4.2: the GCP Secret Manager store for `envoy-infra seed`

**Files:** Create `packages/envoy/infra/seed/gcpstore.go`, `gcpstore_test.go`; modify
`seed/seed.go` and `seed_test.go` (the line for an unreadable latest version), and
`cmd/envoy-infra/main.go` (`--store gcp-secret-manager`).

- [ ] The store implements Task 2.2's `Store` over `cloud.google.com/go/secretmanager/apiv1`
  v1.22.0 (confirm the method names in that version's source first):
  - `Describe` uses `GetSecret`, then `GetSecretVersion` on `versions/latest` for the newest
    version's number and state (metadata, no value read).
  - `Versions` uses `ListSecretVersions`, counting every state, destroyed ones included.
  - `Put` uses `AddSecretVersion`.
  - `Current` reads `versions/latest` with `AccessSecretVersion`.

  `--name` takes `projects/<p>/secrets/<s>`. `--expect-account` is refused for this store. It
  authenticates with Application Default Credentials or `GOOGLE_OAUTH_ACCESS_TOKEN`. Its lines
  name the version Secret Manager serves by default: `check` prints `<name>: absent`,
  `<name>: present, no versions`, `<name>: present; latest: version <n>`, or, when the newest
  version is disabled or destroyed (`versions/latest` names it whatever its state),
  `<name>: present; latest: version <n> is <STATE>`; `verify` prints
  `<name> latest (version <n>) derives <key>: yes`, exit 0, or `…: no`, exit 1. When the latest
  version is unreadable, `verify` fails with the `is <STATE>` line, exit 1 (it wrote nothing, but it
  is a failed check, not a refused input). `Check` and `Verify` read the newest version's number and
  state from `Description.NewestVersion` and `NewestState`, and a `latest` with no version
  (`NewestVersion` empty) is the `no versions` line. Task 2.2 wrote the label as a constant
  (`seed.go` `defaultLabel = "AWSCURRENT"`); this task moves it onto the store (`Store.Label()`:
  `AWSCURRENT` or `latest`), so each store's lines take their own label and shape.
- [ ] Its unit test runs against a gRPC fake server that implements those RPCs, and repeats Task
  2.2's refusal, read-back and canary cases through this store, plus one whose newest version is
  disabled and one whose newest version is destroyed.
- [ ] Commit `feat(envoy-infra): seed command's GCP Secret Manager store`.

### Task 4.3: `envoy-infra:gcp:NatsHub`

**Files:** Create `packages/envoy/infra/gcp/natshub.go`, `cloudconfig.go`, `cloudconfig_test.go`,
`natshub_test.go`, `fetch_key_test.go`, `testdata/cloud-config.golden`

**Interfaces:**

```go
type NatsHubArgs struct {
	Users          []NATSUserArgs     `pulumi:"users"`
	TailnetAuthKey pulumi.StringInput `pulumi:"tailnetAuthKey" provider:"secret"`
	Hostname       string             `pulumi:"hostname"`    // ^[a-z][a-z0-9-]{0,24}[a-z0-9]$ (a DNS label: no trailing hyphen)
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

`users` map onto `core.Authorization`: each user becomes a keyed `core.User` with `HasSeed: true`,
and there is no `noAuthUser`. It is validated and rendered before any registration, with the header
`["Rendered by envoy-infra. Public keys only."]`. The server's other options are command-line
arguments: `-js --store_dir /data -m 8222 --server_name <hostname>`. A user names `machineId` (the
usual case, so no consumer spells Envoy's subjects) or `permissions`. Both or neither is refused,
naming the user.

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
`write_files`: `/etc/nats/nats-server.conf`; `/etc/envoy/fetch-tailnet-key` (COS mounts `/` read-only; `/etc` is writable and executable) writing the key to
`/run/envoy/tailnet-auth-key` (mode 0600) from Secret Manager with the metadata server's token,
taking the metadata host from `GCE_METADATA_HOST` and the Secret Manager base URL from
`SECRET_MANAGER_ENDPOINT` (defaults `metadata.google.internal` and
`https://secretmanager.googleapis.com`) and the output path as its first argument; units
`fetch-tailnet-key` (oneshot), `tailscaled`
(`docker run --rm --name tailscaled --network host --cap-add NET_ADMIN --device /dev/net/tun -v /mnt/disks/data/tailscale:/var/lib/tailscale -v /run/envoy/tailnet-auth-key:/run/envoy/tailnet-auth-key:ro -e TS_AUTHKEY=file:/run/envoy/tailnet-auth-key -e TS_AUTH_ONCE=true -e TS_STATE_DIR=/var/lib/tailscale -e TS_USERSPACE=false -e TS_HOSTNAME=<hostname> -e TS_EXTRA_ARGS=--advertise-tags=<tags> <tailscaleImage>`;
containerboot hands the path to the tailscale CLI inside the container, tailscale v1.104.1
`cmd/containerboot/tailscaled.go:120-121`)
and `nats`
(`docker run --rm --name nats --network host -v /mnt/disks/data/nats:/data -v /etc/nats/nats-server.conf:/etc/nats/nats-server.conf:ro <natsImage> -c /etc/nats/nats-server.conf -js --store_dir /data -m 8222 --server_name <hostname>`,
`ExecStop=/usr/bin/docker stop -t 30 nats`), each with `RequiresMountsFor=/mnt/disks/data`;
`tailscaled` also has `After=` and `Wants=fetch-tailnet-key.service` (not `Requires=`: a node
whose tailnet state is on the disk never reads the key, so a failed fetch on a later boot must not
keep it down); `fetch-tailnet-key` makes up to six attempts, ten seconds apart, and has
`RemainAfterExit=yes`. COS's host firewall accepts incoming connections only for SSH, so a oneshot
`nats-firewall` (`RemainAfterExit=yes`) adds, idempotently (`iptables -w -C … || iptables -w -A …`),
`INPUT -p tcp -s 130.211.0.0/22,35.191.0.0/16 --dport 8222 -j ACCEPT` for Google's health checks;
`nats` has `Wants=` and `After=` it. The tailnet path needs no rule: tailscaled accepts inbound on
`tailscale0` in its own chain (tailscale v1.104.1 `util/linuxfw/iptables_runner.go:229-233`).
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

### Task 4.4: documentation and the slice's proof

- [ ] README and site: `gcp:NatsHub`'s inputs, with a YAML example using placeholders. Also cover
  the permission sets, what the VM runs, and why the stateful disk keeps its tailnet identity.
  Commit `docs(envoy-infra): NATS hub`.
- [ ] **Pre-merge proof.** The slice's acceptance (E8, E9) runs in the first GCP deployment's
  repository, per its plan, against this branch's built plugin and CLI. That plan's tester records
  the result there. This PR gets a summary of outcomes and durations, with placeholders only: no
  project, tailnet, host name or address.

---

## Hardening ledger

Items to verify, each with the row that settles it and where its result goes:

- `maxSurgeFixed: 0` with `maxUnavailableFixed: 1` on a zonal group of one (E9; the first GCP
  deployment's PR).
- `/dev/net/tun` on COS without `modprobe` (E9; same).
- The health check turns HEALTHY through COS's host firewall and the `nats-firewall` rule (E9; same).
- The stateful policy end to end (E9; same).
- The fetch script on COS (its `/bin/sh`, `curl`, `jq`, `base64`, `mktemp`), and the account's
  Secret Manager grant taking effect inside the fetch's retry window on first boot (E9; same).
- `/dev/disk/by-id/google-data` present when `bootcmd` runs, no reformat on a recreate (`blkid -p`
  exits 0), and `waitForInstances: STABLE` within the provider's timeout given autohealing's 300 s
  initial delay (E9; same).
- `NamePrefix` on the instance template under pulumi-gcp's autonaming (E9; same).
- The containerised tailscaled's own accept rule reaching the host, so 4222 is reachable over the
  tailnet through COS's firewall: the tailscale v1.104.1 image writes its `ts-input` accept with
  iptables-legacy, and COS M133 may filter with nft, in which case the legacy accept does not
  override COS's drop policy. E9's first command on the VM is `iptables -V`; under nft, the
  `nats-firewall` unit also accepts tcp/4222 on `tailscale0`, or the tailscale container runs with
  `TS_DEBUG_FIREWALL_MODE=nftables` (E9; same).
- A v4 docker provider diffing unset v5-only inputs (E3 in CI, E4 on the existing deployment).
- The GCP seed store against the real Secret Manager: `versions/latest` names a DISABLED or
  DESTROYED newest version, `GetSecretVersion` on `latest` answers NOT_FOUND with no versions, and
  an unfiltered `ListSecretVersions` includes DESTROYED versions (E8; the first GCP deployment).
- Two statements the README makes from Google's documentation: raising `diskSizeGb` later does not
  grow the kept data disk, and deleting the hub leaves that disk behind, detached (E9; same).

## End-to-end verification plan

| Scenario | Slice | Class | Surface and transport | Identity and grant | Driver |
| --- | --- | --- | --- | --- | --- |
| E1. Unit, vet, notices | 1-4 | infrastructure | GitHub Actions `envoy-infra-go`, `lint` | repo CI | Tasks 1.1, 1.4 |
| E2. `host:Listener` runs a listener that reaches "healthy", with the explicit provider | 1 | infrastructure | `pulumi up` of a YAML program against the runner's Docker, listener image built from the branch | CI runner | Task 1.3 live tests 1, 3 |
| E3. Adoption previews clean | 1 | infrastructure | `pulumi preview --json --show-sames` against the runner's Docker | CI runner | Task 1.3 live test 2 |
| E4. An existing Docker-host deployment moves onto `host:Listener` with no cloud change | 1 (pre-merge), then its consumer's slice | infrastructure | that deployment's own stack and backend, previewed with the branch's plugin binary | its operator (Sami's dotfiles stack; the passphrase is an agent-tier key, no human touch) | the consumer's plan; #1946 gets operation counts by type only |
| E5. The release job's assets and schema, and `pulumi package add` from the release | 1 | infrastructure | the job's shell rehearsed locally; then the real run and Task 1.6 | maintainer | Tasks 1.4, 1.6 |
| E6. Rendered authorization admits and refuses as designed, on 2.10 and on the hub image, and matches the reference renderer's goldens byte for byte | 2 | infrastructure | real nats-server containers | CI runner | Task 2.1 |
| E7. The listener reaches "healthy" under `HostPermissions`, and a session under the same user receives | 4 | infrastructure | real nats-server and this branch's listener binary | CI runner | Task 4.1 live test |
| E8. Seed generate and verify against Secret Manager; a generated seed connects | 2, 4 | infrastructure | in-repo fake stores and real nats-server (Tasks 2.2, 4.2); Secret Manager in the first GCP deployment | that deployment's operator | Tasks 2.2, 4.2, 4.4 |
| E9. `gcp:NatsHub` serves NATS over the tailnet, admits a seeded client, refuses one without, keeps its tailnet identity and stream across `recreate-instances` | 4 | infrastructure | the first GCP deployment, applied from the branch's plugin | its operator; prerequisites: the project's billing linked by the organization's billing administrator, and the operator's tailnet, policy and OAuth-client steps | Task 4.4 |
| E10. The CLI checks and verifies the existing AWS deployment's live seed read-only, printing the reference script's lines | 2 | infrastructure | that deployment's staging Secrets Manager, from the devbox, with the branch-built CLI | the devbox's `staging` profile (`--expect-account` refuses any other) | Task 2.3 |
| E11. The existing AWS deployment's staging, production and dispatch previews create, update, replace or delete no cloud resource, with only state-only creates (component and provider resources) and outputs unchanged | 3 | infrastructure | that deployment's own stacks and backend, from the devbox, with this branch's plugin by path | the devbox's `staging` profile for staging, its instance identity for production and dispatch | Task 3.3; the deployment's own plan |

Legion's own end-to-end stages (`scripts/e2e/stage*.sh`) exercise none of the changed paths. The
Envoy changes (`bus.LocalNATSURL`, `bus.ListenerDurable`/`ListenerQueue`,
`testnats.HubImage`/`StartImageConfig`) are covered by `envoy-go` and `envoy-smoke`, which run on these PRs.

## Skill catalog

Skills available (listed from the directories): Legion `skills/` (`ce-simplify-code`,
`thermonuclear-code-quality`, `thermonuclear-deep-review`, `envoy`, `legion-*`, `dispatch*`); the
dotfiles skills (`opening-a-pr`, `landing-a-pr`, `using-docker`, `updating-docs`,
`review-judgment`, `analyze`, `gh-stack`); superpowers (`test-driven-development`,
`verification-before-completion`, `systematic-debugging`).

| Step | Worker | Required skills |
| --- | --- | --- |
| Tasks 1.1-1.4, 2.1-2.2, 3.1-3.2, 4.1-4.3 | `deep` (implementer) | `test-driven-development`, `using-docker`, `verification-before-completion`, `systematic-debugging` when a test fails unexpectedly |
| Tasks 1.5, 2.3, 3.3, 4.4 (docs) | `deep` | `updating-docs` |
| Opening and landing each PR; Task 1.6 | `deep` (implementer) | `opening-a-pr`, `landing-a-pr`, `gh-stack` (slices 2-3) |
| Slice acceptance | `deep` (tester) | `verification-before-completion`, `using-docker` |
| Task and final review | `reviewer` | `review-judgment` |
| Final pass per PR | coordinator | `ce-simplify-code` (once), then `thermonuclear-deep-review`, `thermonuclear-code-quality` |

## Contract change census

New contracts (component schemas, the CLI) tighten nothing existing. Changes to existing ones:

- `bus.localNATSURL` → `bus.LocalNATSURL`: `git grep -n localNATSURL -- packages/` → `reach.go:38,46,50`
  only. Rename, behaviour unchanged.
- The seed variable's name is exported as `bus.NkeySeedEnvVar` (`"NATS_NKEY_SEED"`), which
  `core.EnvNATSSeed` reuses: `git grep -n NkeySeedEnvVar` → `bus/nkey.go` (the constant and its three
  uses, which spelled the literal before) and `infra/core/listener_env.go`. Value unchanged. Rollout:
  immediate.
- The listener's durable and queue names move into `bus.ListenerDurable`/`ListenerQueue`:
  `git grep -n -e '"listener-"' -e '"envoy-listener-"' -- packages/envoy` → `cmd/listener/main.go:531,634`
  (now call the functions) and the tests `main_test.go:2260,2277`, `reconnect_shutdown_test.go:158`,
  `shutdown_test.go:113`, `startup_test.go:51` (keep their literals as independent checks). The
  names produced are unchanged (Task 4.1 Step 1).
- `release-envoy-listener.yaml` no longer runs for `packages/envoy/infra/**`, and its bump ignores
  it: `git grep -n 'packages/envoy' -- .github/` shows no `workflow_run` or other dependent of that
  workflow. Rollout: immediate.

## Permission source census

| Permission | Holder | Granted by | Enforced by | Test that fails without a holder |
| --- | --- | --- | --- | --- |
| NATS publish/subscribe for a host's listener and sessions | the host's nkey user | the consumer's `users` input to `gcp:NatsHub`, rendered by `core` | nats-server `authorization` | Task 4.1 live test and its negative control |
| NATS publish/subscribe for the existing AWS deployment's users | each keyed user of its authorization | its stack configuration, rendered by `core` through `aws:NatsService` | nats-server `authorization` | E11 (no task-definition change, so the render is byte-identical) |
| `secretsmanager:GetSecretValue` on the API token and the seed | the listener's execution role | `aws:WebhookListener`'s role-policy children | AWS IAM | E11 (policies unchanged) |
| `roles/secretmanager.secretAccessor` on the hub's tailnet auth key | the hub VM's service account | `gcp:NatsHub`'s `SecretIamMember` | Secret Manager IAM | E9 (the VM joins only if it reads the key) |
| Writing a host's seed secret | the operator running `envoy-infra seed generate` | the operator's role in the consumer's account or project (the consumer's plan names it) | Secrets Manager or Secret Manager IAM | E8 |
| `contents: write`, `id-token: write`, `attestations: write`, deploy-key tag push | `release-envoy-infra.yaml`'s `publish` job (the deploy key on its tag step only; `build` holds `contents: read` only) | the job's `permissions` block and `secrets.DEPLOY_KEY` | GitHub | E5 (the first release run) |
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
