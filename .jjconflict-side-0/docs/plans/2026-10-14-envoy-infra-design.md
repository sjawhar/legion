# Envoy infrastructure as reusable Pulumi components

## Why

Envoy needs a NATS server with JetStream and an `envoy-listener` on every host that runs agent
sessions. The listener and Dispatch create the `ENVOY_NOTIFICATIONS` stream and their key-value
buckets themselves at startup (`packages/envoy/internal/bus/stream.go`,
`packages/envoy/internal/store/kv.go`), so a deployment has to supply only a NATS server with an
nkey authorization block and a listener per host. Legion ships the binaries, the image and Compose
files (`packages/envoy/deploy/`), but no infrastructure code, so every operator writes their own.
An operator who runs Envoy for more than one organization writes it once per organization.

This adds Pulumi components that deploy Envoy on GCP, on AWS and on any Docker host. A new
organization's deployment then needs only configuration, not new code.

## Decisions

Sami, 2026-10-13 and 2026-10-14:

- The components are written in Go and ship as an executable Pulumi plugin, so a consumer in any
  Pulumi language needs only the Pulumi CLI.
- They live in their own Go module, `packages/envoy/infra`, nested under `packages/envoy` with a
  `go.work` of its own, not the repository's, so Pulumi and its provider SDKs stay out of the Envoy
  binaries' module and their dependency versions.
- An existing deployment moves onto the components only when its `pulumi preview` creates, updates,
  replaces or deletes no cloud resource. The only creates allowed are the component resources
  themselves, which exist only in Pulumi state. Stack outputs stay unchanged.
- The GCP hub has no webhook ingress in the first version. Webhooks get their own design later.
- Dispatch is out of scope here. It gets its own design.
- The existing deployments move onto the components first, or in parallel with the GCP work, each
  with a preview that shows no diff (Sami, 2026-10-14). The components are generalized from what
  those deployments already run.

## What the module contains

**`core`**, a Go library with no Pulumi resources:

- NATS authorization. Users carry an nkey public key, allow and deny lists for publish and
  subscribe, and an optional `no_auth_user`. `core` validates keys and subject patterns and renders
  the `nats-server` configuration: JetStream store, ports and the authorization block. The
  configuration holds public keys only, never a seed.
- Permission sets for a listener and for an agent session, derived from the subjects Envoy itself
  uses, so a deployment does not hand-write subject lists.
- The listener's environment for a host: machine ID, NATS URLs, the remote-NATS opt-in, port, host
  bridge and seed.

**`envoy-infra seed`**, a command built from the same module:

- `generate --store <store> --name <secret>` creates an nkey user seed, writes it into an existing
  secret that has no versions yet, and prints the public key for the authorization block.
- Stores: GCP Secret Manager now, AWS Secrets Manager with the AWS components. Pulumi declares the
  secret and the command writes its value, so a stack that runs a NATS server never holds a seed:
  the server needs public keys only.

**Pulumi components**, with resource tokens `envoy-infra:<module>:<Name>`:

- `gcp:NatsHub`: single-node NATS JetStream on GCE, reached over a Tailscale tailnet.
  - One VM in a zonal managed instance group of one, autohealed on NATS's `/healthz`.
  - A stateful persistent disk holds the JetStream store and `tailscaled`'s state, so a VM the group
    recreates keeps both its data and its tailnet identity.
  - The VPC firewall admits only Google's health checks. Tailscale needs no inbound rule, so
    clients reach NATS only over the tailnet.
  - Inputs: the authorization, a tailnet auth key (secret), hostname and tags, machine type, disk
    size, the `nats-server` image by digest, and an optional network (created when absent).
  - Outputs: the NATS URL clients use and the instance group.
- `host:Listener`: an `envoy-listener` container on a Docker host, local or over SSH. It pulls the
  pinned image and runs it with host networking, a health check, an ordered shutdown, bounded logs
  and an optional watchdog unit. The container name, port and NATS URLs are inputs, so one host can
  run listeners for two deployments side by side. The seed is a secret input, passed to the
  container as `NATS_NKEY_SEED`; the consumer reads it from its secret store at deploy time, so it
  sits only in that consumer's encrypted stack state.
- `aws:*`: NATS on Fargate with EFS and a tailnet sidecar, and a webhook listener behind a load
  balancer. Their inputs cover every name and setting an existing deployment uses, so moving onto
  them can pass the preview rule above.

Every component creates its resources with the providers the consumer passes in (the `providers`
resource option), never with default providers of the plugin's own versions.

**Release**: a workflow of its own, `release-envoy-infra.yaml`, builds the plugin per OS and architecture from
`.github/envoy-infra-targets.json` (a file of its own, since the shared targets file is a release
input of the other Go binaries) and attaches it to a GitHub release tagged
`envoy-infra-v<version>`, with assets named `pulumi-resource-envoy-infra-v<version>-<os>-<arch>.tar.gz`.
The schema's `pluginDownloadURL` is
`https://github.com/sjawhar/legion/releases/download/envoy-infra-v${VERSION}`: Pulumi fills in
`${VERSION}` and requests that asset name there (`interpolateURL` and `httpSource.Download` in
Pulumi's `sdk/go/common/workspace/plugins.go`). The `github://` scheme is not used, because it looks
only for `v<version>` tags. Consumers install the plugin with `pulumi package add`.

## Order

1. The module and `host:Listener`, with the release job. An existing Docker-host deployment moves
   onto `host:Listener` under the preview rule.
2. In parallel, once the module exists. The two below share their first part, and their second
   parts run side by side:
   - `core`'s NATS authorization and the seed command, then `aws:*`. An existing AWS deployment
     moves onto them under the preview rule.
   - `gcp:NatsHub`, on the same `core` and seed command, for the first GCP deployment.

## Acceptance

- `core`: tests for validation, config rendering and permission sets.
- A test runs a real `nats-server` on a rendered configuration: a client using a seed from
  `envoy-infra seed generate` connects, and a client without one is refused.
- `host:Listener` is applied against a local Docker daemon, and its listener reports healthy against
  a test NATS.
- `gcp:NatsHub` is applied in the first GCP deployment. A client on the tailnet connects with its
  seed. After the instance group recreates the VM, the tailnet name is unchanged and the
  `ENVOY_NOTIFICATIONS` stream still holds its messages.
- Each existing deployment that moves passes the preview rule, for every stack it has.
