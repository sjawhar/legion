# envoy-infra

Pulumi components that deploy Envoy, and a command for the NATS seeds they use. The components
ship as an executable Pulumi plugin, `pulumi-resource-envoy-infra`, so a program in any Pulumi
language needs only the Pulumi CLI. The command, `envoy-infra`, writes a NATS user's nkey seed into
a secret store and checks the seed stored there: see [NATS seeds](#nats-seeds).

This version has two components:

- `envoy-infra:host:Listener`: an `envoy-listener` container on a Docker host. It does not run
  NATS. The listener creates the `ENVOY_NOTIFICATIONS` stream and its key-value buckets itself when
  it starts, so it needs only a NATS server with JetStream to reach.
- `envoy-infra:gcp:NatsHub`: such a server, NATS with JetStream on one Google Cloud VM that joins
  a tailnet, where its clients reach it. See [What `gcp:NatsHub` deploys](#what-gcpnatshub-deploys).

## Install

Each release is the GitHub release tagged `envoy-infra-v<version>`. For linux and darwin, each on
amd64 and arm64, it holds an archive of the plugin and one of the command, with `SHA256SUMS` and
a build provenance attestation for every archive.

### The plugin

Pulumi CLI 3.257.0 or later; the plugin is tested with 3.268.0. `pulumi package add` takes
`--server` from 3.256.0 on, and keeps it in `Pulumi.yaml` from 3.257.0 on.

The plugin runs on the machine that runs `pulumi`, and logs in to the Docker hosts it deploys to, so
verify an archive before installing it. Each release's archives carry build provenance from
`.github/workflows/release-envoy-infra.yaml` on `main`; `--repo` alone would accept an attestation
from any workflow of this repository, so the command pins the signer:

```sh
version=<version>
archive=pulumi-resource-envoy-infra-v$version-<os>-<arch>.tar.gz
gh release download "envoy-infra-v$version" --repo sjawhar/legion --pattern "$archive"
gh attestation verify "$archive" --repo sjawhar/legion \
  --cert-identity https://github.com/sjawhar/legion/.github/workflows/release-envoy-infra.yaml@refs/heads/main \
  --source-ref refs/heads/main --deny-self-hosted-runners
pulumi plugin install resource envoy-infra "$version" \
  --server 'https://github.com/sjawhar/legion/releases/download/envoy-infra-v${VERSION}' \
  --checksum "$(sha256sum "$archive" | cut -d' ' -f1)"
```

`--source-digest <the envoy-infra-v<version> tag's commit>` added to the `verify` pins the commit as
well. `--checksum` makes `pulumi` refuse a download whose SHA-256 is not the archive's you
verified. Then add the package to the project:

```sh
pulumi package add envoy-infra@<version> --server 'https://github.com/sjawhar/legion/releases/download/envoy-infra-v${VERSION}'
```

Keep the single quotes: Pulumi fills in `${VERSION}` itself and downloads
`pulumi-resource-envoy-infra-v<version>-<os>-<arch>.tar.gz` from the GitHub release tagged
`envoy-infra-v<version>`. `pulumi package add` records the package in `Pulumi.yaml`:

```yaml
packages:
  envoy-infra:
    source: envoy-infra
    version: <version>
    pluginDownloadURL: https://github.com/sjawhar/legion/releases/download/envoy-infra-v${VERSION}
```

It also generates an SDK for the project's language under `sdks/` and prints how to import it. A
TypeScript program imports `@sjawhar/envoy-infra`, which `package add` adds to `package.json`. A Go
program imports each component's package under `github.com/sjawhar/envoy/infra/sdk/go/envoyinfra`:
`host` or `gcp`.

### The command

The command's archive is `envoy-infra-<os>-<arch>.tar.gz`, holding the `envoy-infra` binary and
its `THIRD_PARTY_NOTICES` in one directory, `envoy-infra-<os>-<arch>/`. The command writes seeds
with your cloud credentials, so check the archive's provenance before you install it. For linux on
amd64:

```sh
gh release download envoy-infra-v<version> --repo sjawhar/legion --pattern envoy-infra-linux-amd64.tar.gz
gh attestation verify envoy-infra-linux-amd64.tar.gz --repo sjawhar/legion \
  --cert-identity https://github.com/sjawhar/legion/.github/workflows/release-envoy-infra.yaml@refs/heads/main \
  --source-ref refs/heads/main --deny-self-hosted-runners
tar -xzf envoy-infra-linux-amd64.tar.gz
envoy-infra-linux-amd64/envoy-infra --version
```

`--cert-identity` pins the signer to this repository's `release-envoy-infra.yaml` read from `main`,
`--source-ref` to a run started on `main`, and `--deny-self-hosted-runners` to a GitHub-hosted
runner, so an archive attested by any other workflow, branch or runner in the repository fails;
`--repo` alone checks only the repository (`docs/kubernetes.md`, "Pin by digest, never by tag",
explains each flag). `--version` prints the release's version, `<version>`. Put the binary on your
`PATH`; the commands under [NATS seeds](#nats-seeds) call it `envoy-infra`.

To install it with mise, give it a tool alias: `github:sjawhar/legion` is also where the `legion`
command's releases come from, so a second pin of that repository needs a name of its own. Pin the
tag, and name each platform's archive with its digest, since each release holds the plugin's
archives beside the command's:

```toml
[tool_alias]
envoy-infra = "github:sjawhar/legion"

[tools.envoy-infra]
version = "envoy-infra-v<version>"
platforms.linux-x64   = { asset_pattern = "envoy-infra-linux-amd64.tar.gz", checksum = "sha256:<digest>" }
platforms.linux-arm64 = { asset_pattern = "envoy-infra-linux-arm64.tar.gz", checksum = "sha256:<digest>" }
platforms.macos-x64   = { asset_pattern = "envoy-infra-darwin-amd64.tar.gz", checksum = "sha256:<digest>" }
platforms.macos-arm64 = { asset_pattern = "envoy-infra-darwin-arm64.tar.gz", checksum = "sha256:<digest>" }
```

Each `<digest>` is the SHA-256 of that platform's archive: `sha256sum` of the one you verified
above, or GitHub's digest for it, which
`gh api repos/sjawhar/legion/releases/tags/envoy-infra-v<version> --jq '.assets[] | .name, .digest'`
lists. mise strips the archive's one directory, so `envoy-infra` lands at the top of the install
and on `PATH`.

## What `host:Listener` deploys

A listener named `<name>` registers these resources, each a child of the listener:

| Resource | Type | What it is |
| --- | --- | --- |
| `<name>-image` | `docker:index/remoteImage:RemoteImage` | Pulls `image`, and leaves it on the host when the resource is deleted (`keepLocally`). |
| `<name>-container` | `docker:index/container:Container` | The listener: see below. |
| `<name>-watchdog` | `command:remote:Command` | Only with `watchdog`: see [The watchdog](#the-watchdog). |

The container is named `containerName` and runs `/usr/local/bin/envoy-listener` on the host's
network, with:

- restart policy `unless-stopped`;
- 30 seconds to stop after `SIGTERM` (`stopTimeout` and `destroyGraceSeconds`);
- log options `max-file=5` and `max-size=20m`;
- a Docker health check, `curl -f http://127.0.0.1:<port>/healthz` every 10 s, with a 3 s timeout,
  3 retries and a 30 s start period;
- `wait: true` with a 90 s `waitTimeout`, or `wait: false` and no `waitTimeout` under
  `waitForHealthy: false` (see [Readiness](#readiness));
- `deleteBeforeReplace`: a change that replaces the container removes the old one before it
  creates the new one, since both take the same name, so the listener is down in between.

Its environment comes from the inputs:

| Variable | Value |
| --- | --- |
| `PORT` | `port` |
| `ENVOY_MACHINE_ID` | `machineId` |
| `NATS_URLS` | `natsUrls`, joined with commas |
| `ENVOY_ALLOW_REMOTE_NATS` | `1` when any URL in `natsUrls` names a host other than `localhost` or a loopback address. The URL is read, never resolved. |
| `ENVOY_HOST_BRIDGE` | `hostBridge`, when set |
| `NATS_NKEY_SEED` | `seed`, when set. The whole environment is then a secret in the stack's state. |

The listener serves on the host's loopback, `127.0.0.1:<port>`. This version sets no listen host,
no API token and no webhook routes. The listener's other settings are on the documentation site's
[Envoy page](https://sjawhar.github.io/legion/dispatch/envoy/#running-the-listener).

The listener's outputs are `containerName` and `port`.

## Inputs

| Input | Default | What it is |
| --- | --- | --- |
| `machineId` | required | The listener's name, `ENVOY_MACHINE_ID`. Its durable consumer is `listener-<machineId>`. |
| `natsUrls` | required | The NATS servers to connect to. May come from another resource or stack. |
| `image` | required | The listener image, such as `ghcr.io/sjawhar/legion/envoy:<tag>`. |
| `containerName` | `envoy-listener` | The container's name. |
| `port` | `9020` | The port the listener serves and the health check calls. |
| `hostBridge` | unset | The address the listener delivers to this machine's sessions at. Unset, the listener uses `host.docker.internal`. Sessions that serve on the host's loopback are at `127.0.0.1`, since the container shares the host's network. |
| `seed` | unset | A secret: the nkey seed of the NATS user the listener connects as. May come from another resource or stack. |
| `waitForHealthy` | `true` | Whether `pulumi up` waits on the container's health check. See [Readiness](#readiness). |
| `watchdog` | unset | `{host}`: deploys the watchdog on that SSH address. See [The watchdog](#the-watchdog). |
| `adopt` | unset | `{container, image, watchdog}`: root-level resources this listener takes over. See [Moving an existing deployment onto it](#moving-an-existing-deployment-onto-it). |

Only `natsUrls` and `seed` may come from another resource's output. Every other input takes a value
the program knows when it runs, and the plugin refuses an output there, naming the input.

To run two listeners on one host, for two deployments, give each its own `containerName`, `port`
and `machineId`.

`seed` sets `NATS_NKEY_SEED`, the seed itself. Read it from your secret store at deploy time, or
from a secret config value, so it is held only in your stack's encrypted state. Give a listener a
seed only once its NATS server has nkey users: a server with none sends no nonce, and the
listener's NATS client then refuses to connect rather than connect without the seed
(`packages/envoy/deploy/README.md`).

The listener refuses these before it registers anything, with an error naming what is wrong:

- a blank `machineId`;
- a `machineId` or `hostBridge` holding a control character, such as a line break;
- a `port` outside 1-65535;
- a `hostBridge` with leading or trailing whitespace;
- `watchdog` with a `containerName` other than `envoy-listener`;
- `watchdog` with `seed`;
- a `watchdog.host` containing `@` or `://`;
- `watchdog` on a listener whose name contains `"`, `$`, a backtick, `\` or a line break, since the
  watchdog's script echoes the name in double quotes;
- `adopt.watchdog` without `watchdog`, since nothing would take the old command over and the
  update would delete it;
- a missing provider (see [Providers](#providers)).

`natsUrls` and `seed` may come from another resource or stack, so the listener checks them only when
it computes the container's environment. That happens at preview, unless they are not known until
the update. It refuses an empty `natsUrls`, a URL in it that is blank, contains a comma (commas
separate the URLs in `NATS_URLS`) or holds a control character, and a blank `seed`.

The watchdog writes the environment, one variable per line, into a heredoc of a script it runs as
root on the host, so a line break in a value could end the heredoc and run what follows as a
command. That is why no value may hold a control character, even one the program reads from
another stack.

## Providers

A component creates its resources only with providers you pass it in its `providers` resource
option. The listener takes a `docker` provider always, and a `command` provider too when
`watchdog` is set; the hub takes a `gcp` provider. Their resources use that provider at the
version and with the configuration you give it: a docker provider's `host` decides which Docker
daemon the listener runs on, and a gcp provider's `project` which project the hub is created in.
The plugin never falls back to a default provider of its own. Without one, the component fails
before it registers anything:

```text
envoy-infra:host:Listener needs a "docker" provider in its providers option
envoy-infra:gcp:NatsHub needs a "gcp" provider in its providers option
```

## Readiness

By default the docker provider waits up to 90 seconds for the container's health check to pass,
and `pulumi up` fails if it does not. The check passes once `/healthz` answers `200`, which it does
from `starting` on, before the listener has connected to NATS. A listener is connected when
`/healthz` answers `"status":"healthy"`. The answers are listed under
[Health and readiness](https://sjawhar.github.io/legion/dispatch/envoy/#health-and-readiness).

`waitForHealthy: false` creates the container without waiting on its health check. Use it for a
listener that cannot reach its NATS server when the stack is applied: Docker restarts the listener
until it connects.

## The watchdog

`watchdog: {host: <address>}` installs a systemd timer on the host that checks the
`envoy-listener` container 30 seconds after boot and every 60 seconds after that. When the
container is not running, the timer's script runs `docker start` on it if it is `exited` or
`created`, recreates it if it is missing, and removes and recreates it in any other state but
`paused`, such as `restarting`. A `paused` container stays paused: the script runs `docker start`
on it too, which Docker refuses, so every run fails until someone unpauses it. The script's text
is fixed, since any change to it reruns the create command on every host.

`<name>-watchdog` installs it over SSH from the machine running Pulumi, logging in as root, so
`host` is the bare SSH address with no user and no scheme. The component gives the SSH connection
nothing but that address: no key, no password and no host key. pulumi-command v1.2.1 then
authenticates with the keys of the SSH agent at `SSH_AUTH_SOCK` on that machine, and accepts any
host key (`provider/pkg/provider/remote/connection.go`, `SSHConfig`). So that agent must hold a key
root on the host accepts, or the host must accept the login without a key, as Tailscale SSH does.
A key file that the docker provider's `ssh://` connection reads is not enough. The host needs
systemd, Docker and `jq`. Its create command writes:

- `/etc/envoy/listener.env`, the container's environment;
- `/etc/envoy/watchdog.json`, the image, network and health check to recreate the container with;
- `/usr/local/bin/envoy-watchdog`, the script;
- `/etc/systemd/system/envoy-watchdog.service` and `envoy-watchdog.timer`.

Deleting it stops and disables the timer and removes those files.

These files and units are one set per host and name the container `envoy-listener`, so a watchdog
needs that `containerName`. It recreates a removed container from the environment file, which
holds no secret, so a listener with a `seed` cannot have a watchdog: the recreated listener would
come back without its seed.

The create command's text holds the listener's environment, image, port and name, so changing any
of them is an update that runs the create command on the host again.

## Moving an existing deployment onto it

`adopt` names resources at the root of the stack that this listener's resources take over:

| `adopt` field | Takes over | Becomes |
| --- | --- | --- |
| `container` | a `docker:index/container:Container` | `<name>-container` |
| `image` | a `docker:index/remoteImage:RemoteImage` | `<name>-image` |
| `watchdog` | a `command:remote:Command` | `<name>-watchdog` |

Each of the listener's resources gets an alias to the named resource with no parent, so Pulumi
moves that resource's state onto it instead of deleting one and creating the other. The move
changes nothing on the host only when the listener's inputs reproduce the existing resources
exactly, as listed under [What `host:Listener` deploys](#what-hostlistener-deploys), and the
provider you pass it is the one that manages them now or is configured like it.

For a container `listener-example-host-a` and an image `envoy-image-example-host-a` at the root of a
YAML program, the listener that takes both over reads:

```yaml
  example-host-a:
    type: envoy-infra:host:Listener
    properties:
      # ...the inputs that reproduce the existing container...
      adopt:
        container: listener-example-host-a
        image: envoy-image-example-host-a
    options:
      providers:
        - ${docker-example-host-a}
```

A preview proves the move. Apply it only when the preview:

- shows every adopted resource as `same`, and no step for its old root-level name;
- updates, replaces and deletes nothing;
- creates only what exists only in Pulumi state: the listener itself, and provider resources
  (`pulumi:providers:*`) such as the plugin's own default provider,
  `pulumi:providers:envoy-infra`;
- changes no stack output.

This lists every step that is not `same`:

```sh
pulumi preview --json --show-sames | jq -r '.steps[] | select(.op != "same") | "\(.op) \(.urn)"'
```

It does not show a changed stack output, since that leaves the stack's own step `same`. A plain
`pulumi preview` lists each changed output under `Outputs:`.

## What `gcp:NatsHub` deploys

A hub whose `hostname` is `<h>` registers these resources, each a child of the hub and named after
`<h>` rather than after the hub's own name:

| Resource | Type | What it is |
| --- | --- | --- |
| `<h>-hub` | `gcp:serviceaccount/account:Account` | The VM's service account, account id `<h>-hub`. |
| `<h>-tailnet-auth-key` | `gcp:secretmanager/secret:Secret` | The secret `<h>-tailnet-auth-key`, replicated automatically. |
| `<h>-tailnet-auth-key` | `gcp:secretmanager/secretVersion:SecretVersion` | Its version, holding `tailnetAuthKey`. |
| `<h>-tailnet-auth-key-accessor` | `gcp:secretmanager/secretIamMember:SecretIamMember` | The service account's one grant: `roles/secretmanager.secretAccessor` on that secret. |
| `<h>-network` | `gcp:compute/network:Network` | Only without `network`: an auto-mode network, with a subnetwork in every region. |
| `<h>-health-checks` | `gcp:compute/firewall:Firewall` | The hub's one firewall rule: tcp/8222 from Google's health-check ranges, `130.211.0.0/22` and `35.191.0.0/16`, to the VM's network tag `<h>-hub`. |
| `<h>-healthz` | `gcp:compute/healthCheck:HealthCheck` | An HTTP check of NATS's `/healthz` on port 8222, every 10 seconds, unhealthy after 6 failures. |
| `<h>` | `gcp:compute/instanceTemplate:InstanceTemplate` | The VM: `machineType`, a boot disk from Container-Optimized OS's `cos-stable` image family, the data disk, an external address for its way out to the image registries and the tailnet, the service account with the `cloud-platform` scope, the tag `<h>-hub`, and the cloud-config as its `user-data` (see [What the hub's VM runs](#what-the-hubs-vm-runs)). |
| `<h>` | `gcp:compute/instanceGroupManager:InstanceGroupManager` | The zonal group of that one VM, in `zone`. It keeps the data disk (see [The hub's data disk](#the-hubs-data-disk)), recreates the VM once the health check finds it unhealthy, from 300 seconds after the VM is created, and replaces it when the template changes (see [Updating a hub](#updating-a-hub)). It is created after the key's version and the account's grant, since its first VM fetches the key when it boots, and `pulumi up` waits until it is `STABLE`. |

Every resource is in the gcp provider's project. The template's name is `<h>-` and a suffix the
provider adds, and the network, the firewall rule, the health check and the group are named after
their resource names with a random suffix, such as `<h>-healthz-292ee38`. The account id and the
secret id are exact, so two hubs in one project need two hostnames.

The hub's outputs are `natsUrl`, `nats://<h>:4222`, where its clients reach it over the tailnet,
and `instanceGroup`, the group's name.

## The hub's inputs

| Input | Default | What it is |
| --- | --- | --- |
| `users` | required | The NATS server's nkey users. See [The hub's users](#the-hubs-users). |
| `tailnetAuthKey` | required | A secret: the Tailscale auth key the VM joins the tailnet with. The tailnet must let it advertise `tags`. May come from another resource or stack. |
| `hostname` | required | The VM's name on the tailnet and `natsUrl`'s host: 2 to 26 lowercase letters, digits and hyphens, starting with a letter and not ending with a hyphen (`^[a-z][a-z0-9-]{0,24}[a-z0-9]$`). It also names every child. |
| `tags` | required | The tailnet tags the VM advertises when it joins, each `tag:<name>`. At least one. |
| `zone` | required | The zone of the VM and its data disk, such as `us-central1-a`. Changing it makes a new group, with a new data disk: see [Updating a hub](#updating-a-hub). |
| `machineType` | `e2-small` | The VM's machine type. |
| `diskSizeGb` | `20` | The size, in GB, of the data disk the group creates for the hub's first VM. |
| `natsImage` | required | The NATS server image, pinned by digest: `<name>@sha256:<64 lowercase hex digits>`. Its entrypoint takes nats-server's arguments, as the official `nats` image's does. |
| `tailscaleImage` | required | Tailscale's image, pinned the same way. The unit configures it through containerboot's `TS_*` variables, as Tailscale's own image reads them. |
| `network` | created | The name or self link of the VPC network the VM and the firewall rule are in. Unset, the hub creates one. May come from another resource or stack. |
| `subnetwork` | unset | The VM's subnetwork, for a network with custom subnetworks: one in `zone`'s region. Only with `network`. May come from another resource or stack. |

Only `tailnetAuthKey`, `network` and `subnetwork` may come from another resource's output. Every
other input takes a value the program knows when it runs, and the plugin refuses an output there,
naming the input. So the cloud-config is known at preview.

The hub refuses these before it registers anything, with an error naming the input:

- a `hostname` outside the rule above;
- no `tags`, or a tag that is not `tag:` followed by a letter and then letters, digits and
  hyphens, Tailscale's rule for a tag;
- an empty `zone`;
- an image that is not `<name>@sha256:<64 lowercase hex digits>`, with only letters, digits and
  `._/:-` in `<name>`: whitespace, `$` or `%` would split or expand the unit's command;
- an empty `machineType`, or a `diskSizeGb` below 1;
- no `tailnetAuthKey`;
- `subnetwork` without `network`, since the firewall rule needs the network;
- no `users`, or a user refused as [The hub's users](#the-hubs-users) lists;
- a missing provider (see [Providers](#providers)).

## The hub's users

`users` is the NATS server's whole authorization. Each user is `name`, `publicKey` and exactly one
of:

- `machineId`: the user of one host, whose listener and sessions all connect as it, granted
  `HostPermissions(machineId)` (see [Permission sets](#permission-sets)). Give it the listener's
  own `machineId`: the set names that machine's durable consumer and queue group.
- `permissions`: `{publish, subscribe}`, each an `allow` list and an optional `deny` list of
  subject patterns, for any other client.

`publicKey` is the public key `envoy-infra seed generate` prints, and the seed stays in the secret
store it wrote (see [NATS seeds](#nats-seeds)): the host's listener reads it from there into its
`seed`, and its `natsUrls` is the hub's `natsUrl`. In the authorization `core` renders (see
[NATS authorization](#nats-authorization)), each user is a keyed user with `HasSeed` set, and no
user stands for a client presenting no credential, so the server refuses every client without
the seed of one of `users`. The rendered file, headed
`# Rendered by envoy-infra. Public keys only.`, is `/etc/nats/nats-server.conf` on the VM; the
VM's metadata holds it in the clear.

A user is refused, named as `users[<index>] "<name>"`, when it names both `machineId` and
`permissions` or neither, when its `machineId` or `publicKey` is empty, and for each of the
authorization's own refusals under [NATS authorization](#nats-authorization), such as a name of
another form or one two users share, a `publicKey` that is not a user public key (a seed
included), or an empty `allow`.

## What the hub's VM runs

The VM boots Container-Optimized OS (COS), which mounts `/` read-only and keeps `/etc` and
`/mnt/disks` on a tmpfs that does not outlive a boot
([COS file system](https://cloud.google.com/container-optimized-os/docs/concepts/disks-and-filesystem)),
so cloud-init applies the hub's cloud-config at every boot. The whole document, for the hub of
[A NATS hub, in YAML](#a-nats-hub-in-yaml), is `gcp/testdata/cloud-config.golden` in this
directory.

`bootcmd` runs first, in this order:

1. It loads the `tun` module unless `/dev/net/tun` exists.
2. It formats the data disk, `/dev/disk/by-id/google-data`, as ext4 only when `blkid -p` finds no
   filesystem on it (exit status 2) and its first MiB reads, whole, as zeros, as a new persistent
   disk's does. blkid also exits 2 for a filesystem whose primary superblock is damaged, which
   `e2fsck` can repair from a backup superblock, and for a device it cannot open, so any disk but
   a blank one is left as it is, with a line on stderr naming why, such as
   `envoy-infra: /dev/disk/by-id/google-data holds data but no filesystem blkid knows; not formatting it`.
3. It runs `fsck.ext4 -p` on the disk, and mounts it at `/mnt/disks/data` only when fsck left no
   error it could not fix (exit status below 4); otherwise it says so on stderr and leaves it
   unmounted. An unmounted disk keeps NATS and Tailscale down, since their units refuse to start
   without it, until an operator repairs the disk and restarts the VM. It then makes `nats/` and
   `tailscale/` under the mount point.
4. It makes `/run/envoy`, mode 0700, and an empty `/run/envoy/tailnet-auth-key`, mode 0600, so
   the key's bind mount into the Tailscale container is always of a file.

`write_files` then writes the authorization to `/etc/nats/nats-server.conf`, the key fetcher to
`/etc/envoy/fetch-tailnet-key` and four systemd units to `/etc/systemd/system/`, and `runcmd`
reloads systemd and starts the units in this order. They are started and never enabled, since
`/etc` does not outlive the boot.

| Unit | What it does |
| --- | --- |
| `fetch-tailnet-key.service` | A oneshot, after `network-online.target`, that stays active once it exits: `/etc/envoy/fetch-tailnet-key /run/envoy/tailnet-auth-key <h>-tailnet-auth-key`. |
| `tailscaled.service` | `docker run --name tailscaled` of `tailscaleImage` on the host's network, with `NET_ADMIN` and `/dev/net/tun`, its state in `/mnt/disks/data/tailscale`, the key file mounted read-only, `TS_AUTHKEY=file:/run/envoy/tailnet-auth-key`, `TS_AUTH_ONCE=true`, kernel networking (`TS_USERSPACE=false`), `TS_HOSTNAME=<h>` and `--advertise-tags` with `tags`. It wants the fetch and starts after it ends, but does not require it. |
| `nats-firewall.service` | A oneshot that stays active once it exits: it adds `INPUT -p tcp -s 130.211.0.0/22,35.191.0.0/16 --dport 8222 -j ACCEPT` and `INPUT -i tailscale0 -p tcp --dport 4222 -j ACCEPT` to the host's iptables, each unless it is there. COS's host firewall accepts incoming connections for SSH alone ([COS firewall](https://cloud.google.com/container-optimized-os/docs/how-to/firewall)), so without them the health check never reaches NATS, and the tailnet reaches NATS only through the accept rule Tailscale's image writes for `tailscale0`, which a host filtering with another iptables backend overrides. |
| `nats.service` | `docker run --name nats` of `natsImage` on the host's network, with `/mnt/disks/data/nats` as `/data` and the configuration file mounted read-only, and the arguments `-c /etc/nats/nats-server.conf -js --store_dir /data -m 8222 --server_name <h>`. It is stopped with `docker stop -t 30`. It wants `nats-firewall` and starts after it. |

Both container units require `docker.service`, refuse to start unless `/mnt/disks/data` is a mount
point, so that their state never lands on the tmpfs beneath, and restart 10 seconds after their
container exits.

The key fetcher runs under `/bin/sh` with curl, jq and base64. It takes the VM's project id and an
access token for its service account from the metadata server, reads the secret's `latest` version
through Secret Manager's REST API, and moves the decoded key onto `/run/envoy/tailnet-auth-key`
from a temporary file readable by root alone, only when the key is not empty. A new grant can take
a while to reach Secret Manager, so it tries six times, ten seconds apart, and exits 1 after the
sixth failure. Nothing of the key or the token is printed or passed in a command's arguments.
`GCE_METADATA_HOST` and `SECRET_MANAGER_ENDPOINT` replace the metadata server's host and Secret
Manager's base URL, which its tests use.

NATS listens for clients on 4222 and serves its monitoring endpoints on 8222, on every interface.
The hub's VPC firewall rule and `nats-firewall` admit Google's health checks to 8222, and
`nats-firewall` admits the tailnet's interface, `tailscale0`, to 4222; nothing else is opened. So
the tailnet's policy decides which peers reach 4222. 8222's `/varz`, `/connz` and `/jsz` are
reachable over the tailnet only through Tailscale's own accept rule for `tailscale0`, as are its
other ports (see
[What a first Google Cloud deployment proves](#what-a-first-google-cloud-deployment-proves)).

## The hub's data disk

The template's second disk has the device name `data` and outlives its VM (`autoDelete: false`),
and the group holds it as a stateful disk it never deletes (`deleteRule: NEVER`). Google Cloud
keeps a stateful disk through autohealing, updates and recreation and reattaches it to the new VM,
and when the group itself is deleted it detaches the disk and leaves it
([preserved state](https://cloud.google.com/compute/docs/instance-groups/preserved-state)). The
disk holds both containers' state:

- `/mnt/disks/data/nats`, the NATS container's `/data`: JetStream's store, the stream and the
  key-value buckets;
- `/mnt/disks/data/tailscale`, the Tailscale container's `/var/lib/tailscale` (`TS_STATE_DIR`):
  the node's keys and its login.

Keeping the tailnet state there is what lets a recreated VM rejoin the tailnet as the same node,
under the same name, without logging in again. With `TS_AUTH_ONCE=true`, containerboot logs in
with `TS_AUTHKEY` only when the state holds no login (tailscale v1.104.1
`cmd/containerboot/main.go:583-682`), so the VM fetches `tailnetAuthKey` at every boot but logs in
with it only at the first, and a key that is single-use, or has expired since, serves. The same is
why `tailscaled.service` does not require the fetch: on a later boot the fetch may fail and leave
the key file empty, and the node comes up from its state anyway.

The group reattaches the same disk to every later VM, so a later `diskSizeGb`, which changes only
the template, should leave the disk at the size it was created with. Google Cloud's documentation
implies this without saying it, and a first deployment confirms it (see
[What a first Google Cloud deployment proves](#what-a-first-google-cloud-deployment-proves)).
Deleting the hub leaves the disk, detached, in `zone`, as the preserved-state page documents, and
leaves the hub's node on the tailnet; remove both yourself once you no longer need them.

## Updating a hub

An instance template cannot change once it is created
([instance templates](https://cloud.google.com/compute/docs/instance-templates)), and the hub's
holds the cloud-config, so a change to anything the cloud-config is rendered from (`users`,
`tags`, `natsImage`, `tailscaleImage`) or to `machineType`, `diskSizeGb`, `network` or
`subnetwork` makes a new template. The group then replaces its VM at once (`PROACTIVE`, minimal
action `REPLACE`), deleting it before it creates the next (`RECREATE`, no surge, at most one
unavailable). NATS is down from the old VM's stop until the new one's `nats` unit starts, and its
clients reconnect then. The new VM gets the same data disk, so it keeps the stream and rejoins the
tailnet as the same node.

Three inputs work otherwise:

- `tags` reach the tailnet only when the node first logs in. containerboot runs `tailscale up`,
  with `--advertise-tags`, for a node that is not logged in, and otherwise `tailscale set`, which
  passes `--accept-dns=false` and `--hostname` and never `TS_EXTRA_ARGS` (tailscale v1.104.1
  `cmd/containerboot/main.go:583-682`, `cmd/containerboot/tailscaled.go:145-146` and `:169-185`).
  A change to `tags` still replaces the VM; change a joined node's tags on the tailnet itself.
- `hostname` names every child, so a new one is a new hub: Pulumi creates a new set of children,
  whose VM gets a new data disk, so a new tailnet node and an empty stream, then deletes the old
  children, all but the old disk; the old node stays on the tailnet. The new node logs in with
  `tailnetAuthKey`, which must then be a key the tailnet still accepts.
- `zone` replaces the group itself, whose zone cannot change (pulumi-gcp v10.1.0's upstream
  `resource_compute_instance_group_manager.go:120-126`). The group's name is generated, so Pulumi
  creates the new group, under a new name, before it deletes the old one. The new group's VM gets
  a new, empty data disk, so the stream starts empty, and logs in with `tailnetAuthKey`, which must
  then be a key the tailnet still accepts. It asks for `<h>` while the old node still holds it, so
  Tailscale names it `<h>-1` ([machine names](https://tailscale.com/docs/concepts/machine-names)),
  and `natsUrl` names the old node, which goes offline when the old group is deleted. The old disk
  is left detached in the old zone. To move a hub to another zone:

  1. Set `zone`, and a `tailnetAuthKey` the tailnet accepts, and run `pulumi up`. It returns once
     it has deleted the old group, and with it the old VM.
  2. Remove the old node from the tailnet
     ([remove a device](https://tailscale.com/docs/features/access-control/device-management/how-to/remove)).
     Not before `pulumi up` returns: a removed node whose VM still runs logs in again with the key
     that VM fetched at boot, if the key is reusable and unexpired and the tailnet does not
     require device approval, and takes `<h>` back.
  3. Rename the new node from `<h>-1` to `<h>` (Edit machine name, on the machine names page
     above), which also renames it in MagicDNS.

  Until the rename, `natsUrl` names the old, offline node, so the hub's clients reconnect only
  after it, to the new VM's empty stream.

## What a first Google Cloud deployment proves

The unit tests run the hub under Pulumi's mocks, check its cloud-config against the golden, and
run the key fetcher under `/bin/sh` against fakes of the metadata server and Secret Manager; the
seed command's Secret Manager store runs against a fake too. None of them runs Google Cloud, COS
or a tailnet, so the first deployment is where these hold or fail. Watch them on its first
`pulumi up`:

- `/dev/net/tun` exists on COS without `modprobe`, so the Tailscale container starts.
- The data disk is at `/dev/disk/by-id/google-data` when `bootcmd` runs, and a new persistent
  disk's first MiB reads as zeros, so the first boot formats it; `fsck.ext4` runs on COS as its
  documentation shows; and after `gcloud compute instance-groups managed recreate-instances` the
  new VM finds its filesystem (`blkid -p` exits 0) rather than formatting it, and keeps the stream
  and its tailnet node.
- Two statements this README takes from Google Cloud's documentation: a later `diskSizeGb` leaves
  the kept data disk at the size it was created with, and deleting the hub leaves that disk
  behind, detached (see [The hub's data disk](#the-hubs-data-disk)).
- The health check turns `HEALTHY` through COS's host firewall once `nats-firewall` has run, and
  `pulumi up`'s wait for `STABLE` ends within the provider's timeout despite autohealing's
  300-second initial delay. Google Cloud accepts `maxSurgeFixed: 0` with `maxUnavailableFixed: 1`
  on a zonal group of one, and the template's `namePrefix`.
- Clients reach 4222 over the tailnet through `nats-firewall`'s rule, added with COS's own
  `/sbin/iptables` as the health checks' is, whichever backend Tailscale v1.104.1's image writes
  its own accept rule for `tailscale0` with. `iptables -V` on the VM names COS's backend; under
  `nf_tables`, Tailscale's own rule, written with iptables-legacy, does not open 8222 over the
  tailnet.
- The key fetcher runs on COS's `/bin/sh`, curl, jq, base64 and mktemp, and the service account's
  grant reaches Secret Manager within the fetcher's six attempts at the first boot. When every
  attempt fails, containerboot, finding no key, exits at its 60-second boot timeout
  (`TS_BOOT_TIMEOUT`), and `tailscaled.service`'s restart runs the fetch again; that loop was
  measured on systemd 255, not on COS's own.
- `envoy-infra seed` against Secret Manager itself: that `versions/latest` names a newest version
  that is `DISABLED` or `DESTROYED`, that `GetSecretVersion` on `latest` answers `NOT_FOUND` for a
  secret with no version, that `ListSecretVersions` with no filter lists `DESTROYED` versions, and
  that `GetSecret` answers `expire_time` for a secret given a `ttl` or an `expireTime`. The store's
  lines under [What `check` and `verify` print](#what-check-and-verify-print) rest on these. Also
  that `AccessSecretVersion` on `versions/1` answers `NOT_FOUND` for a secret with no version and
  `FAILED_PRECONDITION` for a disabled or destroyed version 1, which `generate`'s check before the
  write rests on; that, after `generate`'s write, `AccessSecretVersion` on a `latest` that names no
  version yet answers `NOT_FOUND`; and how often the listing and `latest` show the write within
  `generate`'s pauses, 7.75 seconds in all. Google documents both as eventually consistent, and
  such operations as converging typically within minutes, but may take a few hours. A write they do
  not show in time ends as an error after writing, though `generate` read version 1 back by number;
  `generate` run again then refuses the secret, which holds version 1, and `check` and `verify`
  confirm the write once `latest` names it.

## Examples

The examples use placeholders: `example-host-a`, `example-operator`, `example-hub`,
`example-project`, the `*.internal.example` hosts, the `example.invalid` images and each `<...>`
stand for your own values.

### A listener, in YAML

The whole `Pulumi.yaml` of a project with one listener, reading its seed from a secret config
value:

```yaml
name: envoy-listeners
runtime: yaml
packages:
  envoy-infra:
    source: envoy-infra
    version: <version>
    pluginDownloadURL: https://github.com/sjawhar/legion/releases/download/envoy-infra-v${VERSION}
config:
  natsSeed:
    type: string
    secret: true
resources:
  docker-example-host-a:
    type: pulumi:providers:docker
    properties:
      host: ssh://root@example-host-a.internal.example
    options:
      version: 4.11.0
  example-host-a:
    type: envoy-infra:host:Listener
    properties:
      machineId: example-host-a
      natsUrls:
        - nats://nats.internal.example:4222
      image: ghcr.io/sjawhar/legion/envoy:<tag>
      hostBridge: 127.0.0.1
      seed: ${natsSeed}
    options:
      providers:
        - ${docker-example-host-a}
outputs:
  containerName: ${example-host-a.containerName}
  port: ${example-host-a.port}
```

In the project's directory, with your Pulumi backend selected:

```sh
pulumi stack init <stack>
pulumi config set --secret natsSeed <seed>
pulumi preview
```

The preview plans `+ 5 to create`: the stack, the docker provider `docker-example-host-a`, the
listener `example-host-a`, and the listener's `example-host-a-image` (a `RemoteImage`) and
`example-host-a-container` (a `Container`). Its outputs are `containerName: "envoy-listener"` and
`port: 9020`. `pulumi up` then applies it.

### A listener, in TypeScript

A listener with a watchdog, taking its NATS URLs from another stack's output. It has no seed,
since a watchdog cannot have one:

```ts
import * as pulumi from "@pulumi/pulumi";
import * as command from "@pulumi/command";
import * as docker from "@pulumi/docker";
import * as envoyInfra from "@sjawhar/envoy-infra";

const nats = new pulumi.StackReference("<organization>/<nats-project>/<stack>");

const dockerHost = new docker.Provider("docker-example-host-a", {
  host: "ssh://root@example-host-a.internal.example",
});
const commandHost = new command.Provider("command-example-host-a");

const listener = new envoyInfra.host.Listener(
  "example-host-a",
  {
    machineId: "example-host-a",
    natsUrls: nats.requireOutput("natsUrls"),
    image: "ghcr.io/sjawhar/legion/envoy:<tag>",
    hostBridge: "127.0.0.1",
    watchdog: { host: "example-host-a.internal.example" },
  },
  { providers: [dockerHost, commandHost] }
);

export const containerName = listener.containerName;
```

### A NATS hub, in YAML

The whole `Pulumi.yaml` of a project with one hub in the project `example-project`, reading the
tailnet auth key from a secret config value. Its users are `example-host-a`, the host whose
listener the first example runs, and `example-operator`, a client with permissions of its own.
Their public keys are the placeholders `core`'s tests use: put in the ones
`envoy-infra seed generate` printed for your users.

```yaml
name: envoy-nats
runtime: yaml
packages:
  envoy-infra:
    source: envoy-infra
    version: <version>
    pluginDownloadURL: https://github.com/sjawhar/legion/releases/download/envoy-infra-v${VERSION}
config:
  tailnetAuthKey:
    type: string
    secret: true
resources:
  gcp-example-project:
    type: pulumi:providers:gcp
    properties:
      project: example-project
    options:
      version: 10.1.0
  example-hub:
    type: envoy-infra:gcp:NatsHub
    properties:
      hostname: example-hub
      zone: us-central1-a
      tags:
        - tag:example-host
      tailnetAuthKey: ${tailnetAuthKey}
      natsImage: example.invalid/nats@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
      tailscaleImage: example.invalid/tailscale@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210
      users:
        - name: example-host-a
          publicKey: UBERXOSHFHFLL7YEXXH7RYEJLK455TF4BOZLIHD5R6NF5FFISNHEXIZO
          machineId: example-host-a
        - name: example-operator
          publicKey: UCYF4CJJNU55NFRALQXGPOAZRM5R5JBXI6OAZB2BHWJ4QTTB5XHFENDG
          permissions:
            publish:
              allow: [">"]
            subscribe:
              allow: [">"]
              deny: ["$SYS.>"]
    options:
      providers:
        - ${gcp-example-project}
outputs:
  natsUrl: ${example-hub.natsUrl}
  instanceGroup: ${example-hub.instanceGroup}
```

The gcp provider is pinned at 10.1.0, the pulumi-gcp release the hub is built against. In the
project's directory, with your Pulumi backend selected:

```sh
pulumi stack init <stack>
pulumi config set --secret tailnetAuthKey <tailnet auth key>
pulumi preview
```

The preview plans `+ 12 to create`: the stack, the gcp provider `gcp-example-project`, the hub
`example-hub`, and the hub's nine children as
[What `gcp:NatsHub` deploys](#what-gcpnatshub-deploys) lists them, the network among them. Its
outputs are `natsUrl: "nats://example-hub:4222"` and `instanceGroup`, `example-hub-` and a random
suffix. The template's `user-data` is `gcp/testdata/cloud-config.golden` byte for byte. `pulumi up`
then applies it. The first example's listener reaches this hub with `natsUrls` set to
`nats://example-hub:4222` and the seed of `example-host-a`.

## NATS seeds

A NATS client authenticates as an nkey user with that user's seed, and the server knows the user
by its public key alone. `envoy-infra seed` keeps the two apart: it writes a new user's seed
straight into a secret store and prints the public key, so the stack that runs the NATS server
holds public keys only. The seed is never an argument, an environment variable, an output, a log
line or part of an error. It has two stores, AWS Secrets Manager and Google Cloud Secret Manager:

```text
envoy-infra seed {check|generate|verify} --store aws-secrets-manager --name <secret id> [--region <r>] [--expect-account <id>] [--public-key U…]
envoy-infra seed {check|generate|verify} --store gcp-secret-manager --name projects/<project>/secrets/<secret> [--public-key U…]
```

With `aws-secrets-manager`, the command runs under your own AWS configuration, as the AWS SDK loads
it: the environment, then the shared configuration and credentials files. `--region` overrides the
region it names. Before any call, it refuses a Secrets Manager endpoint you configure
(`AWS_ENDPOINT_URL`, `AWS_ENDPOINT_URL_SECRETS_MANAGER` or a profile's `services` section) that is
not `https`, unless it is this machine's loopback address, since a seed would cross the network in
plain text; and it retrieves your credentials, and with none it can use, it stops with
`refused: <name>: no usable AWS credentials …`. With `--expect-account`, it then asks STS whose
credentials they are and refuses, before any other call, unless they are that account's. STS and
Secrets Manager each resolve their own endpoint, so the secret is checked too: `DescribeSecret`,
every mode's first call, answers the secret's ARN, and the command refuses a secret in another
region than the one it sends to, and with `--expect-account` one in another account, before it
reads or writes a seed. Pass `--expect-account` whenever you name a secret, so that a seed is never
written to, or read from, a secret of the same name in another account.

With `gcp-secret-manager`, `--name` is the secret's whole name, which names its project, so the
store refuses `--region` and `--expect-account`, and has no flag of its own to expect a project:
every call names the secret by that name, so no call reaches a secret of another project. It checks
the name's form before anything else and refuses a name of any other form, such as a regional
secret's, without printing it, since a seed pasted there must not reach the terminal. It runs under
your
[Application Default Credentials](https://cloud.google.com/docs/authentication/application-default-credentials):
the key file `GOOGLE_APPLICATION_CREDENTIALS` names, then the credentials
`gcloud auth application-default login` saved, then the service account of the Google Cloud
machine it runs on. When `GOOGLE_OAUTH_ACCESS_TOKEN` is set, it runs under that access token
instead, such as one `gcloud auth print-access-token` prints. It calls Secret Manager's gRPC API at
`secretmanager.googleapis.com`, or at the endpoint `SECRET_MANAGER_ENDPOINT` names as
`https://<host>[:<port>]`, a test or private endpoint, whose TLS certificate this machine must
trust. It refuses any other form before any call, `http` on this machine's loopback included: the
client speaks TLS to every endpoint, so a seed is never sent in plain text.

### Making a seed

1. Declare the secret in the consumer's stack, with no value: a secret and no secret version.
   In a Pulumi YAML program:

   ```yaml
   resources:
     nats-seed-example-host-a:
       type: aws:secretsmanager:Secret
       properties:
         name: example/envoy/nats-nkey-seed-example-host-a
   ```

   Apply it. The stack then owns the secret, and no seed ever passes through its state.
2. Check that the secret exists and holds no seed:

   ```sh
   envoy-infra seed check --store aws-secrets-manager --name example/envoy/nats-nkey-seed-example-host-a --expect-account 000000000000
   ```

   ```text
   example/envoy/nats-nkey-seed-example-host-a: present; AWSCURRENT: no
   ```

3. Generate the seed:

   ```sh
   envoy-infra seed generate --store aws-secrets-manager --name example/envoy/nats-nkey-seed-example-host-a --expect-account 000000000000
   ```

   ```text
   public key: U<55 more characters>
   version id: <version id>
   ```

   `generate` makes a new nkey user and writes its seed as the secret's first version. It refuses a
   secret that is absent, is scheduled for deletion or holds any version: one `DescribeSecret` names
   as `AWSCURRENT`, before it lists any, and otherwise any the listing shows, deprecated ones
   included, so it never replaces a seed a client may already read. It prints the public key just
   before the write, so the key is on your terminal whatever happens next, and the version id once
   the write answers. If it cannot print the key, it writes nothing and exits 2. Keep stdout
   somewhere you can read: `>/dev/null`, or a closed stdout, which a Go program sees as
   `/dev/null`, discards the key with nothing to tell. After the write it lists the versions again
   and reads back the version Secrets Manager serves by default (`AWSCURRENT`), which must be the
   one it wrote and derive the public key it printed. Secrets Manager's reads are eventually
   consistent, so while the listing shows no version yet, or no version is `AWSCURRENT`, it looks
   again after pauses of 0.25, 0.5, 1, 2 and 4 seconds; if the write is still not shown, it stops
   with an error after writing. More than one version, or another version served by default, is
   another writer's race.
4. Put the public key into the NATS server's authorization: as a user's `publicKey` on a
   `gcp:NatsHub` (see [The hub's users](#the-hubs-users)), or as a `core.Authorization` user's
   `NKey`, with `HasSeed` set (see [NATS authorization](#nats-authorization)). It is public, so it
   may sit in the stack's configuration in plain text.
5. Verify that the secret serves the seed of that key:

   ```sh
   envoy-infra seed verify --store aws-secrets-manager --name example/envoy/nats-nkey-seed-example-host-a --expect-account 000000000000 --public-key <the public key generate printed>
   ```

   ```text
   example/envoy/nats-nkey-seed-example-host-a AWSCURRENT (version <version id>) derives <the public key>: yes
   ```

On Secret Manager the steps are the same. Declare the secret with no secret version:

```yaml
resources:
  nats-seed-example-host-a:
    type: gcp:secretmanager:Secret
    properties:
      project: example-project
      secretId: envoy-nats-seed-example-host-a
      replication:
        auto: {}
```

and name it in full in each command:

```sh
envoy-infra seed check --store gcp-secret-manager --name projects/example-project/secrets/envoy-nats-seed-example-host-a
envoy-infra seed generate --store gcp-secret-manager --name projects/example-project/secrets/envoy-nats-seed-example-host-a
envoy-infra seed verify --store gcp-secret-manager --name projects/example-project/secrets/envoy-nats-seed-example-host-a --public-key <the public key generate printed>
```

```text
projects/example-project/secrets/envoy-nats-seed-example-host-a: present; latest: no
public key: U<55 more characters>
version id: 1
projects/example-project/secrets/envoy-nats-seed-example-host-a latest (version 1) derives <the public key>: yes
```

`generate` refuses a secret set to expire, since Secret Manager deletes it with every version when
its `expire_time` comes, and a secret that holds any version, whatever its state, destroyed ones
included. Secret Manager's listings and `latest` are eventually consistent with a new version, and
only an access by version number is not
([resource consistency](https://cloud.google.com/secret-manager/docs/access-secret-version)). So
it refuses, naming the version, when `latest` names one, as
`refused: <name> already holds version <n>, which latest names …`; otherwise when the listing shows
any; and, when it shows none, when an access of `versions/1`, the number every secret's first
version gets, finds a version in any state. Then the version it adds must be version 1: any other
number is another writer's version that landed after those reads, and `generate` stops with an
error after writing, naming it. It reads version 1 back by number, which must be the value it
wrote, then `versions/latest`, the version Secret Manager serves by default, which must be the one
it added; while the listing shows no version yet, or `latest` names none, it waits as it does on
Secrets Manager. A `latest` that names a version it will not serve, such as a disabled one, is no
wait but an error after writing. Once a secret holds a second version, `latest` names it and cannot
be pointed back at version 1: disabling or destroying version 2 leaves `latest` naming a version it
will not serve. A version 2 is undone only by adding version 1's seed again, as version 3, or by
moving every client to version 2's key.

The client reads the seed from the store when it is deployed, such as into `host:Listener`'s
`seed` input.

### What `check` and `verify` print

`check` reads the secret's metadata and no value. On AWS Secrets Manager it prints one of:

| Line | The secret |
| --- | --- |
| `<name>: absent` | does not exist. |
| `<name>: present, scheduled for deletion (<date>)` | is deleted and waiting out its recovery window. The date is in this machine's time zone, as `2026-11-01 12:00:00+00:00`, with microseconds after the seconds when it has any. |
| `<name>: present; AWSCURRENT: no` | exists and serves no version by default. `generate` writes to it unless it holds another version, which `check` does not list. |
| `<name>: present; AWSCURRENT: yes (version <id>)` | serves version `<id>` by default. `generate` refuses it. |

On Secret Manager it prints one of:

| Line | The secret |
| --- | --- |
| `<name>: absent` | does not exist. |
| `<name>: present, scheduled for deletion (<date>)` | is set to expire at `<date>`, given as on Secrets Manager: Secret Manager deletes it, with every version, then. `generate` and `verify` refuse it. |
| `<name>: present; latest: no` | exists, and `latest` names no version: it holds none, or `latest` has not caught up with one just added. `generate` writes to it unless a version is listed, or `versions/1` is found. |
| `<name>: present; latest: yes (version <n>)` | serves version `<n>`, its newest, by default. `generate` refuses it. |
| `<name>: present; latest: version <n> is <STATE>` | its newest version, `<n>`, is `DISABLED` or `DESTROYED`. Secret Manager's default, `versions/latest`, names the newest version whatever its state, so the secret serves no value by default, even when an older version is enabled. `generate` refuses it. |

`verify` reads the version the store serves by default, whose label `<label>` is `AWSCURRENT` or
`latest`, and prints one of:

| Line | Exit |
| --- | --- |
| `<name> <label> (version <id>) derives <key>: yes` | 0: the stored seed is the seed of `<key>`. |
| `<name> <label> (version <id>) derives <key>: no` | 1: it is the seed of another user. |
| `<name> <label> (version <id>) is not an nkey user seed: <reason>` | 1: the stored value is no user seed, such as a public key, a seed of another kind or a seed with a line break after it. The reason never quotes the value. |
| `<name>: present; latest: version <n> is <STATE>` | 1, on Secret Manager: the newest version is not enabled, so there is no value to check. `verify` prints this before it reads the value, or, when the version is disabled or destroyed after `verify` looked, once the read is refused for that version's state. |

`verify` refuses a `--public-key` that is not a user public key without printing it, since a seed
pasted there must not reach the terminal.

### Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Done: `check` printed its line, `generate` wrote the seed, read it back and printed the public key and version id, or `verify` printed that the stored seed derives the key. A result that cannot be printed on stdout is never exit 0. |
| 1 | `verify` found another key, no seed, or a newest Secret Manager version that is not enabled; or `generate` failed after it began its write, including a version id it could not print. A version may then exist: `generate` has printed the public key, then the version id, `version id: unknown` when the write never answered, then `error after writing: <what failed>`. An interrupt (Ctrl-C) or `SIGTERM` during the write ends it the same way. Run `check`, and `verify` with that key, before anything else. |
| 2 | Refused before any write, printed as `refused: <reason>`, or a usage error. Nothing was written, so fix what the message names and run it again. A store call that fails before the write, such as one the credentials are not allowed, is a refusal naming the call, the secret and the error's code, AWS's or Secret Manager's gRPC status code, as `refused: describe-secret <name> failed: AccessDeniedException` or `refused: get-secret <name> failed: PermissionDenied`. So is a result `check` or `verify` cannot print, and a public key `generate` cannot print, which stops it before the write: `refused: <name>: could not print the public key: <error>; nothing was written`. |

### The calls each mode makes

A run that is not refused makes exactly these calls, in this order, so its credentials need
exactly these permissions on the secret. On AWS Secrets Manager:

| Mode | Calls | Permissions |
| --- | --- | --- |
| `check` | `DescribeSecret` | `secretsmanager:DescribeSecret` |
| `verify` | `DescribeSecret`, `GetSecretValue` | `secretsmanager:DescribeSecret`, `secretsmanager:GetSecretValue` |
| `generate` | `DescribeSecret`, `ListSecretVersionIds`, `PutSecretValue`, then `ListSecretVersionIds`, and `GetSecretValue` once a version is listed, again after each pause while Secrets Manager has not caught up with the write | `secretsmanager:DescribeSecret`, `secretsmanager:ListSecretVersionIds`, `secretsmanager:PutSecretValue`, `secretsmanager:GetSecretValue` |

So a read-only check of a deployment's seed, `check` and `verify`, needs only `DescribeSecret` and
`GetSecretValue`. `--expect-account` adds `sts:GetCallerIdentity` before them, which needs no
permission ([GetCallerIdentity](https://docs.aws.amazon.com/STS/latest/APIReference/API_GetCallerIdentity.html)).
A secret encrypted with a customer managed KMS key, rather than `aws/secretsmanager`, also needs
`kms:Decrypt` on that key to read it (`verify`, `generate`) and `kms:GenerateDataKey` to write it
(`generate`), as [Secrets Manager's encryption](https://docs.aws.amazon.com/secretsmanager/latest/userguide/security-encryption.html)
describes.

On Secret Manager, where a version call names `versions/latest` unless the table names
`versions/1`:

| Mode | Calls | Permissions |
| --- | --- | --- |
| `check` | `GetSecret`, `GetSecretVersion` | `secretmanager.secrets.get`, `secretmanager.versions.get` |
| `verify` | `GetSecret`, `GetSecretVersion`, `AccessSecretVersion`; when `AccessSecretVersion` answers `FAILED_PRECONDITION`, `GetSecretVersion` again | `secretmanager.secrets.get`, `secretmanager.versions.get`, `secretmanager.versions.access` |
| `generate` | `GetSecret`, `GetSecretVersion`, `ListSecretVersions`, `AccessSecretVersion` on `versions/1`, `AddSecretVersion`, `AccessSecretVersion` on `versions/1`, then `ListSecretVersions` and `AccessSecretVersion`, with `AccessSecretVersion` on `versions/1` between them when the listing shows no version, again after each pause while Secret Manager has not caught up with the write, and `GetSecretVersion` when the last answers `FAILED_PRECONDITION` | `secretmanager.secrets.get`, `secretmanager.versions.get`, `secretmanager.versions.list`, `secretmanager.versions.add`, `secretmanager.versions.access` |

Each permission is the one the API reference names for its call:
[GetSecret](https://cloud.google.com/secret-manager/docs/reference/rest/v1/projects.secrets/get),
[GetSecretVersion](https://cloud.google.com/secret-manager/docs/reference/rest/v1/projects.secrets.versions/get),
[ListSecretVersions](https://cloud.google.com/secret-manager/docs/reference/rest/v1/projects.secrets.versions/list),
[AddSecretVersion](https://cloud.google.com/secret-manager/docs/reference/rest/v1/projects.secrets/addVersion)
and [AccessSecretVersion](https://cloud.google.com/secret-manager/docs/reference/rest/v1/projects.secrets.versions/access).
`verify` stops after `GetSecretVersion` when the newest version is not enabled. When
`AccessSecretVersion` answers `FAILED_PRECONDITION`, the newest version was disabled or destroyed
since: one `GetSecretVersion` names it, for its `is <STATE>` line, exit 1. Any other failed read is
refused, exit 2. Of the
[predefined roles](https://cloud.google.com/secret-manager/docs/access-control), Secret Manager
Viewer (`roles/secretmanager.viewer`) holds `check`'s permissions, `verify` also needs Secret
Manager Secret Accessor (`roles/secretmanager.secretAccessor`), and `generate` needs both and Secret
Manager Secret Version Adder (`roles/secretmanager.secretVersionAdder`).

## NATS authorization

`core.Authorization` is a NATS server's nkey authorization: the users it knows by public key, what
each may publish and subscribe to, and the user a client presenting no credential becomes. A NATS
server's configuration is rendered from it, so it holds public keys only. `gcp:NatsHub` renders
its server's from its `users` (see [The hub's users](#the-hubs-users)); `host:Listener` runs no
NATS server and takes none.

| Field | What it is |
| --- | --- |
| `Users[].Name` | The user's name: lowercase letters, digits and hyphens, 1 to 48 characters, starting and ending with a letter or digit. |
| `Users[].NKey` | The user's public key, `U` and 55 more characters, as `envoy-infra seed generate` prints it. Empty for a pending user. |
| `Users[].Publish`, `Users[].Subscribe` | Each an `Allow` list and an optional `Deny` list of subject patterns. A subscribe entry may instead be `<subject> <queue>`, which scopes it to that queue group. |
| `Users[].HasSeed` | The consumer stores a seed for this user. |
| `NoAuthUser` | Optional: the name of a keyed user without a seed, whom a client presenting no credential becomes. |

A user with no `NKey` is pending: it stands for a seed whose key the server is not told yet, such
as one whose secret is declared but not yet written. It needs `HasSeed`, takes no rules and is not
rendered.

An authorization is refused, with an error naming the field and, for a user's field, the user as
`users[<index>] "<name>"`, when it has:

- no users, or only pending ones: nats-server reads an authorization block with no users as no
  authentication at all;
- a name that is not of the form above, or a name or a key two users share;
- a pending user without `HasSeed`, or with `Publish` or `Subscribe`;
- a keyed user without `Publish` or without `Subscribe`;
- an `NKey` that is not a user public key: `U` and 55 base32 characters, whose bytes are the user
  prefix, the key and a valid CRC-16 checksum. A seed given there is refused too;
- an empty `Allow`: nats-server reads an empty subscribe allow list as no restriction. Write
  `[">"]` to mean everything;
- an entry that is neither a subject pattern nor `<subject> <queue>`. A pattern is dot-separated
  tokens, none empty, with `*` and `>` only as whole tokens and `>` only last, and no whitespace
  or control character anywhere;
- a `<subject> <queue>` entry under `Publish`, since a queue group scopes only subscriptions;
- a `NoAuthUser` that names no user, a pending user or a user with `HasSeed`.

`Render` writes the header lines it is given, each as a `# ` comment, which may not hold a line
break. Then comes the block: a comment naming each keyed user, in order, and the user as one JSON
object, with keys sorted, non-ASCII characters as they are, and `deny` only when it names an
entry. With a `NoAuthUser`, `no_auth_user` follows it, naming that user's key. For three keyed
users, a pending `example-pending` and `NoAuthUser` `example-fallback`, `core`'s tests pin this
text:

```text
# Rendered by envoy-infra from placeholder inputs.
# Public keys only.
authorization {
  users: [
    # example-listener
    {"nkey": "UBERXOSHFHFLL7YEXXH7RYEJLK455TF4BOZLIHD5R6NF5FFISNHEXIZO", "permissions": {"publish": {"allow": ["envoy.example.>"], "deny": ["envoy.example.private"]}, "subscribe": {"allow": ["_INBOX.>", "notifications.café.>"]}}}
    # example-daemon
    {"nkey": "UCYF4CJJNU55NFRALQXGPOAZRM5R5JBXI6OAZB2BHWJ4QTTB5XHFENDG", "permissions": {"publish": {"allow": ["$JS.API.>"]}, "subscribe": {"allow": ["_INBOX.>", "legion.work workers"]}}}
    # example-fallback
    {"nkey": "UDCQRANOFSHHJXENVBTXNFSRUDIUXGXPNWC5ABJFJ7GBPO27F7UIBVXD", "permissions": {"publish": {"allow": [">"]}, "subscribe": {"allow": [">"]}}}
  ]
}
# example-fallback: a client presenting no credential becomes this user
no_auth_user: "UDCQRANOFSHHJXENVBTXNFSRUDIUXGXPNWC5ABJFJ7GBPO27F7UIBVXD"
```

`StartScript` wraps that text in the `sh -c` script a NATS container runs: it writes the text to a
path and execs `nats-server --config <path>` with the server's own arguments. It refuses an
argument that sets `-c`, `-config`, `-user`, `-pass` or `-auth` (with one dash or two, and with or
without `=`), naming its index: nats-server takes the last value of a flag, so a later `-c` would
load another file in place of the rendered one, and the others would add a credential beside its
nkey users.

### Permission sets

`core` also builds the grants Envoy's own clients need from Envoy's own names
(`packages/envoy/internal/bus` and `packages/envoy/internal/contracts`), so no consumer spells
Envoy's subjects. Each is a `core.Permissions`, a publish and a subscribe `Rules` that a user takes
as its `Publish` and `Subscribe`. Below, `<m>` is the machine id with each space as `-`.

`ListenerPermissions(machineID)` is what `envoy-listener` needs as that machine. It publishes to:

- the subjects of the `ENVOY_NOTIFICATIONS` stream (`bus.StreamSubjects()`), the role lanes
  `notifications.role.>` and every exception lane, `notifications.envoy.exceptions.notifications.>`;
- the stream's `$JS.API.STREAM.INFO`, `CREATE` and `UPDATE`, its durable consumer
  `listener-<m>`'s `$JS.API.CONSUMER.INFO` and `CREATE`, the durable's acknowledgements
  `$JS.ACK.ENVOY_NOTIFICATIONS.listener-<m>.>`, and `$JS.API.INFO`;
- for each key-value bucket, `envoy_interests`, `envoy_roles`, `envoy_sessions` and
  `envoy_ci_state`, the calls nats.go's key-value store makes: the info and create of the bucket's
  stream `KV_<bucket>`, its direct and stream message gets, `$KV.<bucket>.>`, and its watchers'
  consumer create and delete and their flow-control replies;
- `$JS.API.STREAM.PURGE.KV_envoy_interests`, which the interest bucket's delete-marker collection
  sends.

It subscribes to `_INBOX.>`, and to the role lanes in the machine's queue group,
`notifications.role.> envoy-listener-<m>`.

`SessionPermissions()` is what any agent session needs, with `*` for the session id. It publishes
to `_INBOX.>`, for the receipt a role delivery waits for, and `agentstream.*.frames`, and
subscribes to `notifications.>` and `agentstream.*.control`.

`HostPermissions(machineID)` is the grant of a host's one NATS user, which its listener and every
session on the host share: in each direction the listener's entries, then each of the session's
the listener's lack. A `gcp:NatsHub` user with `machineId` holds it. This repository's listener
and a session, connected as one user holding it, are checked against real nats-server 2.10 and
2.15.1 servers; `AGENTS.md` in this directory says how.

A plugin release builds the sets from Envoy's names at its own commit, so a listener built from a
later commit that publishes or subscribes to a subject those names lacked needs a hub rendered by
a later release.

## Changing the module

`AGENTS.md` in this directory covers the module's own `go.work`, the rules every component follows,
and how to run the unit and live tests.
