# envoy-infra

Pulumi components that deploy Envoy, and a command for what they need outside Pulumi. The
components ship as an executable Pulumi plugin, `pulumi-resource-envoy-infra`, so a program in any
Pulumi language needs only the Pulumi CLI:

| Component | What it deploys |
| --- | --- |
| `envoy-infra:host:Listener` | An `envoy-listener` container on a Docker host: see [host:Listener](#hostlistener). |
| `envoy-infra:aws:NatsService` | A NATS server with JetStream on ECS Fargate, reachable in the VPC through Cloud Map and on a tailnet through a Tailscale sidecar: see [The AWS components](#the-aws-components). |
| `envoy-infra:aws:WebhookListener` | Envoy's webhook listener on ECS Fargate, behind your load balancer's target group, with an alarm on each log line that says a delivery or a start went wrong. |

The listener creates the `ENVOY_NOTIFICATIONS` stream and its key-value buckets itself when it
starts, so it needs only a NATS server with JetStream to reach.

The command, `envoy-infra`, writes a NATS user's nkey seed into a secret store and checks the seed
stored there ([NATS seeds](#nats-seeds)), and prints what the AWS components register for a program
that needs it outside Pulumi ([`envoy-infra render`](#envoy-infra-render)).

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
program imports `github.com/sjawhar/envoy/infra/sdk/go/envoyinfra/host` and `.../envoyinfra/aws`. A
Python program imports `sjawhar_envoy_infra`, the generated `sjawhar-envoy-infra` package.

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
`PATH`; the commands below call it `envoy-infra`.

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

## host:Listener

### What `host:Listener` deploys

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

### Inputs

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

- an nkey seed of any type, in any spelling listed under "What `check` and `verify` print",
  anywhere in its name or in any input but `seed`: `machineId`, `image`, `containerName`,
  `hostBridge`, `watchdog.host`, `adopt.container`, `adopt.image` or `adopt.watchdog`. The error
  names the input and never the value, as `envoy-infra:host:Listener example-host-a: image holds an
  nkey seed (not echoed)`, since each would reach a resource's inputs, an alias or another refusal.
  `seed` is the one input that is a seed;
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
separate the URLs in `NATS_URLS`), holds a control character or holds an nkey seed anywhere, as a
password in its userinfo or a host split across DNS labels would, and a blank `seed`.

The watchdog writes the environment, one variable per line, into a heredoc of a script it runs as
root on the host, so a line break in a value could end the heredoc and run what follows as a
command. That is why no value may hold a control character, even one the program reads from
another stack.

### Providers

The listener creates its resources only with providers you pass it in its `providers` resource
option: a `docker` provider always, and a `command` provider too when `watchdog` is set. Its
resources use that provider at the version and with the configuration you give it, and the
provider's `host` decides which Docker daemon the listener runs on. The plugin never falls back to a
default provider of its own. Without one, the listener fails before it registers anything:

```text
envoy-infra:host:Listener needs a "docker" provider in its providers option
```

### Readiness

By default the docker provider waits up to 90 seconds for the container's health check to pass,
and `pulumi up` fails if it does not. The check passes once `/healthz` answers `200`, which it does
from `starting` on, before the listener has connected to NATS. A listener is connected when
`/healthz` answers `"status":"healthy"`. The answers are listed under
[Health and readiness](https://sjawhar.github.io/legion/dispatch/envoy/#health-and-readiness).

`waitForHealthy: false` creates the container without waiting on its health check. Use it for a
listener that cannot reach its NATS server when the stack is applied: Docker restarts the listener
until it connects.

### The watchdog

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

### Moving an existing deployment onto it

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

[A preview proves the move](#proving-a-move-with-a-preview).

### Examples

The examples use placeholders: `example-host-a`, the `*.internal.example` hosts and each `<...>`
stand for your own values.

#### YAML

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

#### TypeScript

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

## The AWS components

`aws:NatsService` and `aws:WebhookListener` run Envoy on ECS Fargate in a VPC you already have.
Your network, ECS cluster, roles, security groups, private DNS namespace, load balancer and alarm
topic stay in your program; the components take them as inputs. A program usually runs both, the
listener connecting to the NATS service's `internalUrl`, as the [Python example](#python-example)
does.

### What `aws:NatsService` deploys

A NATS service named `<N>` registers these resources, each a child of the component:

| Resource | Type | What it is |
| --- | --- | --- |
| `<N>-efs` | `aws:efs/fileSystem:FileSystem` | JetStream's store: encrypted, `generalPurpose`, `elastic` throughput. |
| `<N>-efs-sg` | `aws:ec2/securityGroup:SecurityGroup` | Its mount targets' group, in `vpcId`. |
| `<N>-efs-ingress`, `<N>-nfs-egress` | `aws:ec2/securityGroupRule:SecurityGroupRule` | NFS (TCP 2049) from `securityGroupId` into that group, and out of `securityGroupId` to it. |
| `<N>-efs-mt-<i>` | `aws:efs/mountTarget:MountTarget` | One in each subnet of `subnetIds`, `<i>` its index there. |
| `<N>-efs-ap` | `aws:efs/accessPoint:AccessPoint` | `/nats-data`, owned by uid and gid 1000 (the `nats` image's user), mode 755. |
| `<N>-log-group` | `aws:cloudwatch/logGroup:LogGroup` | `logGroupName`, kept `logRetentionDays` days. |
| `<N>-tsnet-efs`, `-tsnet-efs-sg`, `-tsnet-efs-ingress`, `-tsnet-nfs-egress`, `-tsnet-efs-mt-<i>`, `-tsnet-efs-ap` | as above | The sidecar's tailnet state, so its node keeps its identity across tasks: the same shape, with the access point at `/tsnet-state`, owned by root, mode 700. |
| `<N>-task` | `aws:ecs/taskDefinition:TaskDefinition` | `family`, `cpu` and `memory` on Fargate (`awsvpc`), with the two containers below and both file systems mounted through their access points with transit encryption. |
| `cloudMapServiceLogicalName` | `aws:servicediscovery/service:Service` | `cloudMapServiceName` in the namespace: an `A` record, TTL 10 s, `MULTIVALUE`. |
| `<N>-service` | `aws:ecs/service:Service` | One task on Fargate `platformVersion`, in `subnetIds` with `securityGroupId` and no public address, registered in Cloud Map, with ECS Exec on, created after every mount target. It deploys at 0% minimum and 100% maximum, so the old task stops before the new one starts: two tasks would share the JetStream store and the tailnet node. |
| `<prefix>-route-disconnect-filter`, `-slow-consumer-filter`, `-jetstream-error-filter`, `-auth-error-filter` | `aws:cloudwatch/logMetricFilter:LogMetricFilter` | `<prefix>` is `metricFilterLogicalNamePrefix`; see below. |

Each tagged child carries `baseTags` and a `service` tag: `<N>` on the JetStream file system and
its access point, the log group, the task definition, the Cloud Map service and the ECS service;
`<N>-efs` on the JetStream group; `<N>-tsnet`, `<N>-tsnet-efs` and `<N>-tsnet-ap` on the
tailnet-state file system, group and access point. The rules, mount targets and filters carry none.

The task runs two containers, both logging to the log group through `awslogs` in `region`:

- `nats`, `natsImage`: clients on 4222, cluster routes on 6222 and monitoring on 8222, JetStream's
  store at `/data`, and a health check fetching `http://localhost:8222/healthz`. Without
  `authorization` the image's entrypoint runs `nats-server` with `serverArgs`. With it, `/bin/sh -c`
  runs a start script that writes the rendered configuration, public keys only
  ([NATS authorization](#nats-authorization)), to `configPath` and execs
  `nats-server --config <configPath>` with `serverArgs`.
  [`envoy-infra render nats-start-script`](#nats-start-script) prints that script.
- `tailscale-sidecar`, `tailscaleImage`: serves the NATS client port on the tailnet as
  `tailnetHostname`, advertising `tailnetTags`, with its state at `/var/lib/tailscale`. Its auth key
  is the OAuth client secret at `tailnetClientSecretArn`, which ECS injects, with
  `?ephemeral=<tailnetEphemeral>&preauthorized=true` appended.

The filters count nats-server's own log lines into metrics in `metricNamespace`, 1 for each
matching line and 0 for a period whose lines all miss:

| Filter | Pattern | Metric |
| --- | --- | --- |
| `route-disconnect` | `"Router connection closed"` | `RouteDisconnections` |
| `slow-consumer` | `"Slow Consumer Detected"` | `SlowConsumers` |
| `jetstream-error` | `?"JetStream out of" ?"JetStream failed to store" ?"JetStream being DISABLED" ?"NO quorum, stalled"` | `JetStreamErrors` |
| `auth-error` | `?"authentication error" ?"Subscription Violation" ?"Publish Violation"` | `AuthErrors` |

Its outputs are `serviceArn` (the ECS service's ARN), `serviceName`, `efsId` (JetStream's file
system), `logGroupName`, `taskDefinitionArn`, `metricNamespace` and `internalUrl`,
`nats://<cloudMapServiceName>.<cloudMapNamespaceName>:4222`.

### `aws:NatsService`'s inputs

| Input | Default | What it is |
| --- | --- | --- |
| `authorization` | unset | The server's nkey authorization, an `aws:Authorization` ([NATS authorization](#nats-authorization)). Unset, NATS runs with no authorization block. |
| `configHeader` | with `authorization` | The rendered configuration's header lines, each written as a `# ` comment; `[]` for none. Required with `authorization` and refused without it, since only its start script writes a configuration. |
| `serverArgs` | required | `nats-server`'s arguments: JetStream, its store directory, the ports. With `authorization` they follow `--config <configPath>`. |
| `configPath` | with `authorization` | Where the start script writes the configuration in the container. Required with `authorization` and refused without it. |
| `natsImage`, `tailscaleImage` | required | The two containers' images. |
| `tailnetHostname` | required | The sidecar's node name, `TS_HOSTNAME`. |
| `tailnetTags` | required | The tags the node advertises, joined with `,` into `--advertise-tags`: at least one, since an auth key made from an OAuth client must advertise one. |
| `tailnetClientSecretArn` | required | The secret holding the tailnet OAuth client secret. |
| `tailnetEphemeral` | `false` | Whether the node's auth key is ephemeral. |
| `region` | required | The containers' `awslogs-region`. |
| `vpcId` | required | The VPC the file systems' groups are in. |
| `subnetIds` | required | The task's subnets, at least one. Each file system has a mount target in each, named by its index. |
| `securityGroupId` | required | The NATS task's group, which you build. |
| `clusterArn`, `executionRoleArn`, `taskRoleArn` | required | The ECS cluster and the task's two roles. |
| `cloudMapNamespaceId`, `cloudMapNamespaceName` | required | The private DNS namespace the service registers in. |
| `cloudMapServiceName` | required | The service's name in it. |
| `cloudMapServiceLogicalName` | required | The Cloud Map service's logical name. |
| `family`, `cpu`, `memory`, `platformVersion` | required | The task definition's family and size, and the service's Fargate platform version. |
| `logGroupName`, `logRetentionDays` | required | The log group. |
| `baseTags` | required | Tags on every tagged child, beside `service`, which the component sets. |
| `metricNamespace` | required | The namespace of the filters' metrics. |
| `metricFilterLogicalNamePrefix` | required | What the filters' logical names start with. |
| `waitForSteadyState` | unset | The ECS service's `waitForSteadyState`, set only when given. |
| `adopt` | `false` | See [Moving an existing deployment onto them](#moving-an-existing-deployment-onto-them). |

### What `aws:WebhookListener` deploys

A listener named `<N>` registers these resources, each a child of the component:

| Resource | Type | What it is |
| --- | --- | --- |
| `<N>-log-group` | `aws:cloudwatch/logGroup:LogGroup` | `logGroupName`, kept `logRetentionDays` days. |
| `cloudMapServiceLogicalName` | `aws:servicediscovery/service:Service` | `cloudMapServiceName` in the namespace: an `A` record, TTL 10 s, `MULTIVALUE`. |
| `<N>-execution-api-token-read` | `aws:iam/rolePolicy:RolePolicy` | On `executionRoleName`: `secretsmanager:GetSecretValue` on `apiTokenSecretArn`. |
| `<N>-execution-nats-nkey-seed-read` | `aws:iam/rolePolicy:RolePolicy` | The same on `seedSecretArn`, only when it is given. |
| `<N>-task` | `aws:ecs/taskDefinition:TaskDefinition` | `family`, `cpu` and `memory` on Fargate (`awsvpc`), with the one container below, created after both policies and after `apiTokenSecretVersionId`'s resource. |
| `<N>-service` | `aws:ecs/service:Service` | One task in `subnetIds` with `securityGroupId` and no public address, its port 9020 in `targetGroupArn`, registered in Cloud Map, with ECS Exec on, created after `natsServiceArn`'s resource. It deploys at 100% minimum and 200% maximum, so a healthy target stays in the target group; the circuit breaker rolls back a replacement that keeps failing to start, the service waits for steady state, ECS ignores load-balancer health for 120 s while a task starts, and create and update time out after 15 minutes. |
| `<N>-<filter>` | `aws:cloudwatch/logMetricFilter:LogMetricFilter` | Eight, on the log group; `envoy-infra render metric-filters` prints them. |
| `<N>-<alarm>` | `aws:cloudwatch/metricAlarm:MetricAlarm` | Five, below. |

The log group, the Cloud Map service, the task definition, the service and the alarms carry
`baseTags` and `service: <N>`; the policies and the filters carry none.

The container, `envoy-listener`, runs `/usr/local/bin/envoy-listener` from `image` on port 9020.
Its health check fetches `/healthz` on the task's own address, the one the target group probes. Its
environment, in this order:

| Variable | Value |
| --- | --- |
| `PORT` | `9020` |
| `ENVOY_LISTEN_HOST` | `listenHost` |
| `ENVOY_MACHINE_ID` | `machineId` |
| `NATS_URLS` | `natsUrls`, joined with commas |
| `ENVOY_ALLOW_REMOTE_NATS` | `1` when any URL names a host other than `localhost` or a loopback address |
| `ENVOY_HOST_BRIDGE` | `hostBridge`, when it is not `""` |
| `ENVOY_NATS_REPLICAS` | `natsReplicas` |
| `ENVOY_WEBHOOKS` | `webhooks`, joined with commas, when it lists one |
| `ENVOY_GITHUB_MENTION_TRIGGER`, `ENVOY_REVIEWER_APP_ID` | `mentionTrigger`, `reviewerAppId`, with `github` |

ECS injects its secrets from their ARNs: `ENVOY_GITHUB_WEBHOOK_SECRET` with `github`,
`ENVOY_SLACK_SIGNING_SECRET` with `slack`, `ENVOY_API_TOKEN`, and `NATS_NKEY_SEED` with
`seedSecretArn`.

Each alarm pages `alarmTopicArn` on one matching line in five minutes (`Sum` over 300 s at least 1,
missing data not breaching) and sends the OK transition there too, so the channel sees the signal
stop: `<N>-publish-failure` (a JetStream publish that did not complete), `<N>-webhook-refused` (a
delivery NATS can never take, answered 422), `<N>-stream-subject-dropped` (a start that dropped
another deployment's overlapping stream subject), `<N>-durable-refused` (a start that refused its
durable consumer) and `<N>-subscribe-exhausted` (a start that gave up binding its durable
subscription). Each description names `logGroupName` as the log to read, and assumes the wiring of
the deployment the alarms were written for: that GitHub deliveries come from Dispatch's GitHub App
(`legion-envoy`), whose redelivery sweep retries them, and that Slack retries an event three times.
A listener fed otherwise pages the same way, with descriptions that overstate what is retried.

Its outputs are `serviceArn`, `serviceName`, `logGroupName`, `taskDefinitionArn` and
`internalUrl`, `http://<cloudMapServiceName>.<cloudMapNamespaceName>:9020`.

### `aws:WebhookListener`'s inputs

| Input | Default | What it is |
| --- | --- | --- |
| `machineId` | required | `ENVOY_MACHINE_ID`, the listener's name; its durable consumer is `listener-<machineId>`. |
| `natsUrls` | required | The NATS servers. May come from another resource as an output of the whole list, such as `nats.internal_url.apply(lambda url: [url])`. |
| `image` | required | The listener image, such as `ghcr.io/sjawhar/legion/envoy@sha256:<digest>`. |
| `region` | required | The container's `awslogs-region`. |
| `apiTokenSecretArn` | required | The secret holding the listener's API token. |
| `apiTokenSecretVersionId` | required | The token's secret version. The component only orders the task definition after it, so pass your `SecretVersion`'s `versionId` output: a literal orders nothing. |
| `seedSecretArn` | unset | The secret holding the seed of the NATS user the listener connects as. Unset, it connects with none. |
| `webhooks` | required | The webhooks the listener serves: `["github", "slack"]`, or `[]` for none. With `[]` the container carries no webhook's setting or secret, and the listener mounts no webhook route. This version takes no other list. |
| `githubWebhookSecretArn`, `reviewerAppId`, `mentionTrigger` | with `github` | Required while `webhooks` lists `github`, refused while it does not. |
| `slackSigningSecretArn` | with `slack` | Required while `webhooks` lists `slack`, refused while it does not. |
| `clusterArn`, `executionRoleArn`, `taskRoleArn` | required | The ECS cluster and the task's two roles. |
| `executionRoleName` | required | The execution role's name, which the read policies attach to. |
| `subnetIds` | required | The task's subnets, at least one. |
| `securityGroupId` | required | The listener task's group, which you build. |
| `targetGroupArn` | required | Your load balancer's target group. |
| `cloudMapNamespaceId`, `cloudMapNamespaceName`, `cloudMapServiceName`, `cloudMapServiceLogicalName` | required | As for `aws:NatsService`. |
| `family`, `cpu`, `memory` | required | The task definition's family and size. |
| `logGroupName`, `logRetentionDays` | required | The log group, which the filters read and the alarms' descriptions name. |
| `listenHost` | required | `ENVOY_LISTEN_HOST`. The listener binds `127.0.0.1` when it is blank, where the target group cannot reach it. |
| `hostBridge` | required | `ENVOY_HOST_BRIDGE`; `""` leaves it unset. |
| `natsReplicas` | required | `ENVOY_NATS_REPLICAS`, at least 1. |
| `alarmTopicArn` | required | The SNS topic the alarms page and clear on. |
| `baseTags`, `metricNamespace` | required | As for `aws:NatsService`. |
| `natsServiceArn` | required | The NATS service's ARN. The component only orders its ECS service after it, so the listener rolls out after NATS, which by then holds the key the listener presents: pass the `NatsService`'s `serviceArn` output, not a literal. |
| `adopt` | `false` | See [Moving an existing deployment onto them](#moving-an-existing-deployment-onto-them). |

### What the AWS components refuse

Each component refuses these before it registers anything, with an error naming the component and
the input:

- Either, first: a name, or any plain string input, that holds an nkey seed of any type and in any
  spelling (`bus.HoldsSeed`), named by its path (`authorization.users[1].name`, `tailnetTags[1]`,
  `baseTags[env]`, or `a key of baseTags`) and never quoted, as `<path> holds an nkey seed (not
  echoed)`. Every one of them reaches a refusal, a registered name, tag or alias, a resource's
  inputs or the container definitions. An authorization's `nkey` is such an input too, and core
  also refuses any nkey that is not a user public key without quoting it.
- `aws:NatsService`: a `configHeader` or `configPath` missing while `authorization` is given (an
  empty `configPath` counts as missing) or given while it is not, each checked before the
  authorization; then, as `StartScript` checks them (see
  [NATS authorization](#nats-authorization)), an `authorization` it refuses, a `serverArgs` entry
  that sets nats-server's `-c`, `-config`, `-user`, `-pass`, `-auth`, `-cluster`,
  `-cluster_listen` or `-routes`, and a `configHeader` line holding a line break; an empty
  `subnetIds`; an empty `tailnetTags`; a `baseTags` holding `service`.
- `aws:WebhookListener`: a `webhooks` not given at all (give `[]` for a listener with no webhook);
  a webhook's input missing while `webhooks` lists it (a blank `reviewerAppId` or `mentionTrigger`
  counts as missing) or given while it does not, each input checked before the list; then a
  `webhooks` other than `[]` or exactly `["github", "slack"]`; a blank `machineId`; a `machineId`,
  `hostBridge` or `listenHost` holding a control character; a `hostBridge` with leading or trailing
  whitespace; a blank `listenHost`; a `natsReplicas` below 1; an empty `subnetIds`; a `baseTags`
  holding `service`.
- Either, without an `aws` provider (see [Providers and transformations](#providers-and-transformations)).

`natsUrls` may come from another resource, so the listener checks it when the container's
definitions are computed, at preview unless it is not known until the update: it refuses an empty
list, and a URL that is blank, holds a comma or holds a control character. An optional secret ARN
that is given but resolves to no value is refused the same way. Every input that takes an output,
the secret ARNs included, is refused once it resolves to a value holding a seed, naming its path,
so no child is registered with it.

### Inputs that take an output

The inputs a program takes from its other resources are Pulumi inputs: the IDs, ARNs and names of
your resources, the optional secret ARNs, `natsUrls`, and each element of `subnetIds`. Every other
input takes a value the program knows when it runs, and the plugin refuses another resource's
output there, naming the input, before the component registers anything. The schema marks those
inputs `plain`, but only partly: pulumi-go-provider's inference marks a list's or a map's elements
and not the list or map, and nothing in an object type. So a generated SDK takes an output for
`authorization`, `configHeader`, `serverArgs`, `tailnetTags`, `webhooks` and `baseTags`, and the
plugin refuses it:

```text
envoy-infra:aws:NatsService resource 'example-nats' has a problem: failed to copy inputs for example-nats (envoy-infra:aws:NatsService): copying input "configHeader": expected destination type to implement pulumi.Input or pulumi.Output, got []string
```

`subnetIds` is a list of inputs, not an input of a list: each subnet ID may be an output, but the
list itself must be known when the program runs, since each component counts the subnets to name
their mount targets and place the service. The generated SDK accepts one output for the whole
list, such as `subnet_ids=vpc.private_subnet_ids`, and the plugin refuses it:

```text
copying input "subnetIds": expected destination type to implement pulumi.Input or pulumi.Output, got []pulumi.StringInput
```

Pass a list with one entry per subnet, each entry a subnet's ID or its output.

### Secrets stay yours

The components take each secret as the ARN of a Secrets Manager secret you declare: the
listener's API token, the NATS user's seed, the webhook secrets and the tailnet OAuth client. They
create no secret and no random value, and no secret's value passes through their inputs or Pulumi
state: ECS injects each into its container from the ARN. The listener's policies let its execution
role read the token and, with `seedSecretArn`, the seed. The execution roles must already be able to
read the others: the tailnet OAuth client and the GitHub and Slack secrets.

### Every deployment value is an input

The components hold no deployment's names. Each child's logical name comes from the component's
name, the Cloud Map services' from `cloudMapServiceLogicalName` and the NATS filters' from
`metricFilterLogicalNamePrefix`; physical names, tags, metric namespaces and the configuration's
header lines are inputs. What Envoy fixes stays fixed in the component: the ports, the containers'
names, commands and health checks, the volumes and mount paths, the sidecar's script, the filters'
patterns and the alarms' shape and text.

The AWS provider normalises the order and whitespace of a task definition's
`containerDefinitions`, but compares each string in them byte for byte. The components write the
container definitions as Python's `json.dumps` writes them, and the start script, the sidecar's
script and the health checks exactly as the deployment they were written for writes them, so that
deployment's task definitions preview as `same` once it moves onto them.

### Providers and transformations

Each component creates its resources with the `aws` provider you pass it in its `providers`
option, at the version and with the configuration you give it, and never falls back to a default
provider of the plugin's own SDK version. Without one, it fails before it registers anything:

```text
envoy-infra:aws:NatsService needs a "aws" provider in its providers option
```

A component's children are registered by the plugin, not by your program. `protect` on the
component reaches each of them, since a child inherits it, and so does a resource transform
(`transforms`, or `register_resource_transform` in Python). A stack transformation registered with
the legacy API (`transformations`, `register_stack_transformation`) runs only on your program's
own resources, so write an option meant for a child as a transform or on the component.

### Moving an existing deployment onto them

With `adopt: true`, each child gets an alias to the root-level resource of its own name and type,
with no parent, so Pulumi moves that resource's state onto the child instead of deleting one and
creating the other. The move changes nothing in AWS only when:

- each child's logical name is the existing resource's: the component's name, and
  `cloudMapServiceLogicalName` and `metricFilterLogicalNamePrefix`, reproduce the existing names;
- `subnetIds` lists the subnets in their existing order, since subnet `<i>`'s mount targets are
  `<N>-efs-mt-<i>` and `<N>-tsnet-efs-mt-<i>`;
- every input reproduces the existing resources, the container definitions byte for byte;
- the `aws` provider you pass is configured exactly as the one that manages them now. Moving a
  resource from the default provider to an explicit one is decided by the explicit provider's diff
  of its configuration, so give it every setting the default provider has, its region included.
  The preview then also creates that provider, which exists only in Pulumi state.

[A preview proves the move](#proving-a-move-with-a-preview).

### Moving back

The move is one-way. After an update with `adopt: true`, the stack's state holds only the
children, so a program that drops the component and declares the old root-level resources again
plans to create each of them and delete each child, both EFS file systems included. To move back,
either:

- give each root-level resource an alias to its child: the same name and type, with the component
  as its parent (in Python, `pulumi.Alias(parent="<the component's URN>")`); or
- move the children's state out and back with `pulumi state move`: to a scratch stack, where each
  becomes a root-level resource, then back to the stack. Moving them within one stack
  (`--source` and `--dest` the same) removes them from its state.

Prove either with a preview, as for the move onto the components. Until you no longer need a way
back, set `protect` on the component: each child inherits it, so Pulumi refuses to delete one.
Before the move back, unprotect the component alone (`pulumi state unprotect <its URN>`), so the
update can delete it from state.

### Python example

A program running both components, with its own API token. The `example-*` values and the
`*.internal.example` hosts stand for your own; each string standing for one of your network's,
cluster's, roles' or load balancer's resources would be that resource's output:

```python
import pulumi
import pulumi_aws as aws
import sjawhar_envoy_infra as envoy_infra

config = pulumi.Config()
region = pulumi.Config("aws").require("region")

# Configure this provider as your default aws provider is configured.
aws_provider = aws.Provider("envoy-aws", region=region)
components = pulumi.ResourceOptions(providers=[aws_provider])

arn = f"arn:aws:secretsmanager:{region}:000000000000:secret:example/envoy/"
cluster_arn = f"arn:aws:ecs:{region}:000000000000:cluster/example-cluster"
execution_role_arn = "arn:aws:iam::000000000000:role/example-execution"
task_role_arn = "arn:aws:iam::000000000000:role/example-task"
subnet_ids = ["subnet-0example0000000a", "subnet-0example0000000b"]
base_tags = {"project": "example-project", "env": "example-env"}

api_token = aws.secretsmanager.Secret("example-api-token", name="example/envoy/api-token")
api_token_version = aws.secretsmanager.SecretVersion(
    "example-api-token-version",
    secret_id=api_token.id,
    secret_string=config.require_secret("apiToken"),
)

nats = envoy_infra.aws.NatsService(
    "example-nats",
    authorization=envoy_infra.aws.AuthorizationArgs(
        users=[
            envoy_infra.aws.UserArgs(
                name="example-listener",
                nkey="UBERXOSHFHFLL7YEXXH7RYEJLK455TF4BOZLIHD5R6NF5FFISNHEXIZO",
                publish=envoy_infra.nats.RulesArgs(allow=[">"]),
                subscribe=envoy_infra.nats.RulesArgs(allow=[">"]),
                has_seed=True,
            ),
        ],
    ),
    config_header=["Rendered by envoy-infra.", "Public keys only."],
    server_args=["--jetstream", "--store_dir", "/data", "--port", "4222", "--http_port", "8222"],
    config_path="/tmp/nats-server.conf",
    nats_image="example.registry/nats:<tag>",
    tailscale_image="example.registry/tailscale:<tag>",
    tailnet_hostname="example-bus",
    tailnet_tags=["tag:example-bus"],
    tailnet_client_secret_arn=arn + "tailnet-oauth",
    region=region,
    vpc_id="example-vpc",
    subnet_ids=subnet_ids,
    security_group_id="example-nats-sg",
    cluster_arn=cluster_arn,
    execution_role_arn=execution_role_arn,
    task_role_arn=task_role_arn,
    cloud_map_namespace_id="example-namespace-id",
    cloud_map_namespace_name="example-namespace.internal.example",
    cloud_map_service_name="example-bus",
    cloud_map_service_logical_name="example-nats-cloudmap",
    family="example-nats",
    log_group_name="/ecs/example-nats",
    log_retention_days=30,
    cpu="256",
    memory="1024",
    platform_version="LATEST",
    base_tags=base_tags,
    metric_namespace="example/NATS",
    metric_filter_logical_name_prefix="example-nats",
    opts=components,
)

listener = envoy_infra.aws.WebhookListener(
    "example-listener",
    machine_id="example-fargate",
    nats_urls=nats.internal_url.apply(lambda url: [url]),
    image="example.registry/envoy@sha256:<digest>",
    region=region,
    api_token_secret_arn=api_token.arn,
    api_token_secret_version_id=api_token_version.version_id,
    seed_secret_arn=arn + "nats-nkey-seed-example-listener",
    webhooks=["github", "slack"],
    github_webhook_secret_arn=arn + "github-webhook",
    slack_signing_secret_arn=arn + "slack-signing",
    reviewer_app_id="1234567",
    mention_trigger="@legion",
    cluster_arn=cluster_arn,
    execution_role_arn=execution_role_arn,
    execution_role_name="example-execution",
    task_role_arn=task_role_arn,
    subnet_ids=subnet_ids,
    security_group_id="example-listener-sg",
    target_group_arn=f"arn:aws:elasticloadbalancing:{region}:000000000000:targetgroup/example-hooks/0123456789abcdef",
    cloud_map_namespace_id="example-namespace-id",
    cloud_map_namespace_name="example-namespace.internal.example",
    cloud_map_service_name="example-hooks",
    cloud_map_service_logical_name="example-listener-cloudmap",
    family="example-listener",
    log_group_name="/ecs/example-listener",
    log_retention_days=30,
    cpu="256",
    memory="512",
    listen_host="0.0.0.0",
    host_bridge="127.0.0.1",
    nats_replicas=1,
    alarm_topic_arn=f"arn:aws:sns:{region}:000000000000:example-alarms",
    base_tags=base_tags,
    metric_namespace="example/EnvoyListener",
    nats_service_arn=nats.service_arn,
    opts=components,
)

pulumi.export("natsUrl", nats.internal_url)
pulumi.export("listenerUrl", listener.internal_url)
```

With `aws:region` and the secret `apiToken` set, `pulumi preview` plans `+ 47 to create`: the stack,
the explicit provider, the token's secret and version, the two components and their 22 and 19
children. Its outputs are `natsUrl: "nats://example-bus.example-namespace.internal.example:4222"`
and `listenerUrl: "http://example-hooks.example-namespace.internal.example:9020"`.

## Proving a move with a preview

A preview proves a move onto a component. Apply it only when the preview:

- shows every adopted resource as `same`, and no step for its old root-level name;
- updates, replaces and deletes nothing;
- creates only what exists only in Pulumi state: the components themselves, and provider resources
  (`pulumi:providers:*`) such as an explicit provider you pass a component and the plugin's own
  default provider, `pulumi:providers:envoy-infra`;
- changes no stack output.

This lists every step that is not `same`:

```sh
pulumi preview --json --show-sames | jq -r '.steps[] | select(.op != "same") | "\(.op) \(.urn)"'
```

It does not show a changed stack output, since that leaves the stack's own step `same`. A plain
`pulumi preview` lists each changed output under `Outputs:`.

## `envoy-infra render`

`envoy-infra render` prints what the AWS components register, for a program that needs it outside
Pulumi, such as a check that runs a real NATS server on the start script or one that tests the
filters' patterns against log lines. Each subcommand builds its output with the function the
component builds it with, and a flag sets the component's input its `--help` names. A value the
component refuses is refused, naming the flag, as `refused: --<flag>: <reason>` with exit 2; a usage
error prints the usage and exits 2. It reads only its flags and the file `--authorization-file`
names, and calls no API.

An nkey seed of any type is never echoed. A flag value that holds one (`bus.HoldsSeed`: whole or
inside it, in either case, split over `.`, `-`, `_` or whitespace, percent-encoded or not) is
refused before anything reads it, naming the flags and never the value (`refused: --<flag>: an nkey
seed, which is never an argument`). So is a seed anywhere in the `--authorization-file`, naming
its field (`users[0].name`, `users[0].publish.allow[0]`), or the object whose key holds it. A flag
the command does not take, and a value a numeric or boolean flag cannot parse, are reported
without quoting what was given, as `envoy-infra seed` reports them.

### nats-start-script

```text
envoy-infra render nats-start-script --authorization-file <json> [--config-header <line>]... [--server-arg <arg>]... --config-path <path>
```

Prints the script `aws:NatsService`'s `nats` container runs under `/bin/sh -c`, exactly as the
component registers it, with no newline after it. The file holds the component's `authorization`
input as JSON, keyed as the schema spells it (`users`, each with `name`, `nkey`, `publish` and
`subscribe` with `allow` and `deny`, and `hasSeed`; `noAuthUser`); a key it does not have, a key in
another letter case, or anything after the one JSON value, is refused. Give `--config-header` once
per line, none for the component's `configHeader: []`, and `--server-arg` once per argument, in
order. For a file with one user:

```json
{"users": [{"name": "example-listener", "nkey": "UBERXOSHFHFLL7YEXXH7RYEJLK455TF4BOZLIHD5R6NF5FFISNHEXIZO", "publish": {"allow": [">"]}, "subscribe": {"allow": [">"]}, "hasSeed": true}]}
```

```text
$ envoy-infra render nats-start-script --authorization-file authorization.json --config-header 'Rendered by envoy-infra.' --server-arg --jetstream --server-arg --store_dir --server-arg /data --config-path /tmp/nats-server.conf
set -eu; printf '%s' '# Rendered by envoy-infra.
authorization {
  users: [
    # example-listener
    {"nkey": "UBERXOSHFHFLL7YEXXH7RYEJLK455TF4BOZLIHD5R6NF5FFISNHEXIZO", "permissions": {"publish": {"allow": [">"]}, "subscribe": {"allow": [">"]}}}
  ]
}
' > /tmp/nats-server.conf; exec nats-server --config /tmp/nats-server.conf --jetstream --store_dir /data
```

### webhook-listener-env

```text
envoy-infra render webhook-listener-env --machine-id <id> --nats-url <url>... [--webhook <name>]... [--reviewer-app-id <id>] [--mention-trigger <t>] --listen-host <h> --host-bridge <h> --nats-replicas <n>
```

Prints `aws:WebhookListener`'s container environment, one `KEY=value` line per variable in the
container's order. The secrets ECS injects are not printed. It refuses what the component refuses of
these inputs, and a value holding a line break, which no line could carry:

```text
$ envoy-infra render webhook-listener-env --machine-id example-fargate --nats-url nats://example-bus.example-namespace.internal.example:4222 --webhook github --webhook slack --reviewer-app-id 1234567 --mention-trigger @legion --listen-host 0.0.0.0 --host-bridge 127.0.0.1 --nats-replicas 1
PORT=9020
ENVOY_LISTEN_HOST=0.0.0.0
ENVOY_MACHINE_ID=example-fargate
NATS_URLS=nats://example-bus.example-namespace.internal.example:4222
ENVOY_ALLOW_REMOTE_NATS=1
ENVOY_HOST_BRIDGE=127.0.0.1
ENVOY_NATS_REPLICAS=1
ENVOY_WEBHOOKS=github,slack
ENVOY_GITHUB_MENTION_TRIGGER=@legion
ENVOY_REVIEWER_APP_ID=1234567
```

With no `--webhook`, it prints no `ENVOY_WEBHOOKS` line and no GitHub lines, as the component writes
none for `webhooks: []`, and refuses `--reviewer-app-id` and `--mention-trigger`.

Without `--reviewer-app-id`, it prints
`refused: --reviewer-app-id: reviewerAppId is required with webhook "github"` and exits 2.

### metric-filters

```text
envoy-infra render metric-filters --component nats-service|webhook-listener --metric-namespace <ns> --logical-name-prefix <p> --json
```

Prints a component's log metric filters as a JSON array, in the order the component registers
them, each with its `logicalName`, `pattern`, `metricName` and `metricNamespace`.
`--logical-name-prefix` is what their logical names start with: an `aws:NatsService`'s
`metricFilterLogicalNamePrefix`, an `aws:WebhookListener`'s own name. `--json` is required: JSON is
the one output format. The listener's first filter, for a listener named `example-listener`:

```json
{
  "logicalName": "example-listener-publish-failure-filter",
  "pattern": "{ $.level = \"ERROR\" && $.msg = \"listener exception publish failed\" }",
  "metricName": "PublishFailures",
  "metricNamespace": "example/EnvoyListener"
}
```

## NATS seeds

A NATS client authenticates as an nkey user with that user's seed, and the server knows the user
by its public key alone. `envoy-infra seed` keeps the two apart: it writes a new user's seed
straight into a secret store and prints the public key, so the stack that runs the NATS server
holds public keys only. The seed is never an argument, an environment variable, an output, a log
line or part of an error. This version has one store, AWS Secrets Manager:

```text
envoy-infra seed {check|generate|verify} --store aws-secrets-manager --name <secret id> [--region <r>] [--expect-account <id>] [--public-key U…]
```

The command runs under your own AWS configuration, as the AWS SDK loads it: the environment, then
the shared configuration and credentials files. `--region` overrides the region it names. Before
any request or credential retrieval, it refuses a seed anywhere in that configuration or in the
endpoints it resolves from it, without echoing it (see "What `check` and `verify` print"); then a
Secrets Manager endpoint you configure (`AWS_ENDPOINT_URL`, `AWS_ENDPOINT_URL_SECRETS_MANAGER` or
a profile's `services` section) that is not `https`, unless it is this machine's loopback address,
since a seed would cross the network in plain text; and it retrieves your credentials, and with
none it can use, it stops with `refused: <name>: no usable AWS credentials …`. With
`--expect-account`, it then asks STS whose credentials they are and refuses, before any other
call, unless they are that account's; an empty `--expect-account`, as `--expect-account
"$ACCOUNT"` becomes with the variable unset, is a usage error, never a skipped check. STS and
Secrets Manager each resolve their own endpoint, so the secret is checked too: `DescribeSecret`,
every mode's first call, answers the secret's ARN, and the command refuses a secret in another
region than the one it sends to, and with `--expect-account` one in another account, before it
reads or writes a seed.
Pass `--expect-account` whenever you name a secret, so that a seed is never written to, or read
from, a secret of the same name in another account.

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
   secret that is absent, is scheduled for deletion, holds any version, deprecated ones included, or
   is already served by default, even when the versions listing, which Secrets Manager answers
   eventually consistently, does not show it yet, so it never replaces a seed a client may already
   read. Every write uses one fixed `ClientRequestToken`, so a second run's first-version write with
   another seed, racing this one, is refused (`ResourceExistsException`, exit 2) rather than written
   beside it, and the key that run printed is not stored. It prints the public key just before the
   write, so the key is on your terminal whatever happens next, and the version id once the write
   answers. If it cannot print the key, it writes nothing and exits 2. With stdout on the null
   device it refuses before any call, exit 2, since the key would be lost: `>/dev/null`, and a
   closed stdout, which a Go program sees as `/dev/null`. After the write it lists the versions again
   and reads back the version Secrets Manager serves by default (`AWSCURRENT`), which must be the
   one it wrote and derive the public key it printed. Secrets Manager's reads are eventually
   consistent, so while the listing shows no version yet, or no version is `AWSCURRENT`, it looks
   again after pauses of 0.25, 0.5, 1, 2 and 4 seconds; if the write is still not shown, it stops
   with an error after writing. More than one version, or another version served by default, is
   another writer's race.
4. Put the public key into the NATS server's authorization as that user's `nkey`, with `hasSeed`
   set: see [NATS authorization](#nats-authorization). It is public, so it may sit in the stack's
   configuration in plain text.
5. Verify that the secret serves the seed of that key:

   ```sh
   envoy-infra seed verify --store aws-secrets-manager --name example/envoy/nats-nkey-seed-example-host-a --expect-account 000000000000 --public-key <the public key generate printed>
   ```

   ```text
   example/envoy/nats-nkey-seed-example-host-a AWSCURRENT (version <version id>) derives <the public key>: yes
   ```

The client reads the seed from the store when it is deployed, such as into `host:Listener`'s
`seed` input.

### What `check` and `verify` print

`check` reads the secret's metadata and no value. It prints one of:

| Line | The secret |
| --- | --- |
| `<name>: absent` | does not exist. |
| `<name>: present, scheduled for deletion (<date>)` | is deleted and waiting out its recovery window. The date is in this machine's time zone, as `2026-11-01 12:00:00+00:00`, with microseconds after the seconds when it has any. |
| `<name>: present; AWSCURRENT: no` | exists and serves no version by default. `generate` writes to it unless it holds another version, which `check` does not list. |
| `<name>: present; AWSCURRENT: yes (version <id>)` | serves version `<id>` by default. |

`verify` reads the `AWSCURRENT` version and prints one of:

| Line | Exit |
| --- | --- |
| `<name> AWSCURRENT (version <id>) derives <key>: yes` | 0: the stored seed is the seed of `<key>`. |
| `<name> AWSCURRENT (version <id>) derives <key>: no` | 1: it is the seed of another user. |
| `<name> AWSCURRENT (version <id>) is not an nkey user seed: <reason>` | 1: the stored value is no user seed, such as a public key, a seed of another kind or a seed with a line break after it. The reason never quotes the value. |

`verify` refuses a `--public-key` that is not a user public key without printing it, since a seed
pasted there must not reach the terminal. Before any store opens, the command refuses a flag whose
value holds an nkey seed, in any of the spellings below (`--name`, `--region`, or any other),
naming the flag and not the value: `--name` is every line's subject and the secret id the store is
sent, and `--region` names the endpoint the SDK resolves. An unknown mode, an argument after the
flags and a flag the command does not take are not echoed either.

The AWS store refuses a seed anywhere the AWS configuration carries one into a client, the
configuration before any client is built and the endpoints before any request:

- any `AWS_` environment variable, every one the SDK reads among them (`AWS_REGION`,
  `AWS_PROFILE`, `AWS_CONFIG_FILE`, `AWS_ENDPOINT_URL` and `AWS_ENDPOINT_URL_<SERVICE>`,
  `AWS_ACCESS_KEY_ID`, `AWS_SESSION_TOKEN`, `AWS_ROLE_ARN`, `AWS_SDK_UA_APP_ID`,
  `AWS_CONTAINER_CREDENTIALS_FULL_URI` and the rest), and `_X_AMZN_TRACE_ID`, which the SDK sends
  as a header on Lambda;
- the default paths of the shared config and credentials files, under `$HOME`;
- every value of the shared config profile the SDK reads, and of each `source_profile` it names:
  `region`, `source_profile`, `role_arn`, `role_session_name`, `external_id`, `mfa_serial`,
  `credential_source`, `credential_process`, `web_identity_token_file`, `sso_session` and its
  `sso-session` section's `sso_region` and `sso_start_url`, `endpoint_url` and each `services`
  section's `endpoint_url`, the credentials file's keys, `sdk_ua_app_id`, `ca_bundle`, and any
  other;
- the Secrets Manager and STS endpoints, as the SDK resolves them.

A seed of any nkey type (operator, server, cluster, account, user or curve) is found in any
spelling a reader or a decoder takes for it: whole or inside a longer value, such as a path or any
part of a URL; in either case; with its last character's two unused bits changed, which nkeys
itself reads; padded with `=`; split by `.`, `-`, `_` or whitespace, as across DNS labels or
wrapped lines; or percent-encoded up to three times, which covers a URL nested in a URL. A seed
percent-encoded more often, split over two values, or split by any other character, is not found:
the check decodes three times at most, so its cost stays linear in the value, and neither part of
a split seed decodes as a seed. The refusal names where the value came from, never the value, as
`refused: <name>: AWS_ENDPOINT_URL: an nkey seed, which is never AWS configuration (not echoed)`
or `refused: <name>: the shared config's profile "example": source_profile: …`.

### Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Done: `check` printed its line, `generate` wrote the seed, read it back and printed the public key and version id, or `verify` printed that the stored seed derives the key. A result that cannot be printed on stdout is never exit 0. |
| 1 | `verify` found another key or no seed; or `generate` failed after it began its write, including a version id it could not print, but not a write Secrets Manager refused, which is exit 2. A version may then exist: `generate` has printed the public key, then the version id, `version id: unknown` when the write never answered, then `error after writing: <what failed>`. An interrupt (Ctrl-C) or `SIGTERM` during the write ends it the same way. Run `check`, and `verify` with that key, before anything else. |
| 2 | Refused before any write, printed as `refused: <reason>`, or a usage error. Nothing was written, so fix what the message names and run it again. A store call that fails before the write, such as one the credentials are not allowed, is a refusal naming the call, the secret and the AWS error code, as `refused: describe-secret <name> failed: AccessDeniedException`. So is a result `check` or `verify` cannot print, and a public key `generate` cannot print, which stops it before the write: `refused: <name>: could not print the public key: <error>; nothing was written`. So is the first write Secrets Manager refuses because another `generate` already wrote this secret's first version (`ResourceExistsException`): it comes after the public key is printed, but no version holds that key, and no `version id` line follows: `refused: put-secret-value <name> failed: ResourceExistsException; another generate wrote this secret's first version, with another seed; nothing was written, and the public key above is not stored`. Run `check`, then `verify` with the other run's key. |

### The AWS calls each mode makes

A run that is not refused makes exactly these calls, in this order, so its credentials need
exactly these permissions on the secret:

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

## NATS authorization

`core.Authorization` is a NATS server's nkey authorization: the users it knows by public key, what
each may publish and subscribe to, and the user a client presenting no credential becomes. A NATS
server's configuration is rendered from it, so it holds public keys only. `aws:NatsService` takes
it as its `authorization` input, an `aws:Authorization` of `aws:User`s, with the properties below;
`envoy-infra render nats-start-script` reads the same from its file, keyed the same way. A user's
`publish` and `subscribe` are each a `nats:Rules`, the one rules type every NATS component takes,
whatever its cloud: `nats.RulesArgs` in the Python and Go SDKs.

| Input | What it is |
| --- | --- |
| `users[].name` | The user's name: lowercase letters, digits and hyphens, 1 to 48 characters, starting and ending with a letter or digit. |
| `users[].nkey` | The user's public key, `U` and 55 more characters, as `envoy-infra seed generate` prints it. Empty for a pending user. |
| `users[].publish`, `users[].subscribe` | Each an `allow` list and an optional `deny` list of subject patterns. A subscribe `allow` entry may instead be `<subject> <queue>`, which scopes it to that queue group. |
| `users[].hasSeed` | The consumer stores a seed for this user. |
| `noAuthUser` | Optional: the name of a keyed user without a seed, whom a client presenting no credential becomes. |

A user with no `nkey` is pending: it stands for a seed whose key the server is not told yet, such
as one whose secret is declared but not yet written. It needs `hasSeed`, takes no rules and is not
rendered.

An authorization is refused, with an error naming the field and, for a user's field, the user as
`users[<index>] "<name>"`, when it has:

- a name, an `allow` or `deny` entry (a queue group included) or a `noAuthUser` that holds an nkey
  seed, in any spelling listed under "What `check` and `verify` print". This is checked first, and
  the error names the field by its path and never the value, as `users[0] "example-listener":
  publish.allow[1] holds an nkey seed (not echoed)`, or `users[0]: name holds …` for a name: every
  other refusal quotes a name or an entry, and the block writes them;
- no users, or only pending ones: nats-server reads an authorization block with no users as no
  authentication at all;
- a name that is not of the form above, or a name or a key two users share;
- a pending user without `hasSeed`, or with `publish` or `subscribe`;
- a keyed user without `publish` or without `subscribe`;
- an `nkey` that is not a user public key: `U` and 55 base32 characters, whose bytes are the user
  prefix, the key and a valid CRC-16 checksum. A seed given there is refused too;
- an empty `allow`: nats-server reads an empty subscribe allow list as no restriction. Write
  `[">"]` to mean everything;
- an entry that is neither a subject pattern nor `<subject> <queue>`. A pattern is dot-separated
  tokens, none empty, with `*` and `>` only as whole tokens and `>` only last, and no whitespace
  or control character anywhere;
- a `<subject> <queue>` entry under `publish`, since a queue group scopes only subscriptions;
- a `<subject> <queue>` entry in `subscribe.deny`, which nats-server before 2.15 enforces more
  loosely than it reads: beside a plain deny of the same subject it re-allows every other queue
  group, and alone it misses a wildcard subscription that covers its subject. Deny the plain
  subject, or narrow `subscribe.allow`;
- a `noAuthUser` that names no user, a pending user or a user with `hasSeed`.

`Render` writes the header lines it is given, each as a `# ` comment, which may not hold a line
break or an nkey seed. Then comes the block: a comment naming each keyed user, in order, and
the user as one JSON object, with keys sorted, non-ASCII characters as they are, and `deny` only
when it names an entry. With a `noAuthUser`, `no_auth_user` follows it, naming that user's key.
For three keyed users, a pending `example-pending` and `noAuthUser` `example-fallback`, `core`'s
tests pin this text:

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
path and execs `nats-server --config <path>` with the server's own arguments. It refuses a path or
an argument that holds an nkey seed, which the script would carry, naming it and never the
value, and an argument that sets `-c`, `-config`, `-user`, `-pass`, `-auth`, `-cluster`,
`-cluster_listen` or `-routes` (with one dash or two, and with or without `=`), naming its index:
nats-server takes the last value of a flag, so a later `-c` would load another file in place of
the rendered one; `-user`, `-pass` and `-auth` would add a credential beside its nkey users; and
the rendered block has no cluster authorization, so a route the last three open or solicit would
be unauthenticated, and a server routed to it would see every message its users publish. Each of
its refusals is a `core.InputError` naming the input it refuses: `authorization`, `configPath`,
`serverArgs` or `configHeader`.

## Changing the module

`AGENTS.md` in this directory covers the module's own `go.work`, the rules every component follows,
and how to run the unit and live tests.
