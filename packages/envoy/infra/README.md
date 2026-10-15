# envoy-infra

Pulumi components that deploy Envoy, and a command for the NATS seeds they use. The components
ship as an executable Pulumi plugin, `pulumi-resource-envoy-infra`, so a program in any Pulumi
language needs only the Pulumi CLI. The command, `envoy-infra`, writes a NATS user's nkey seed into
a secret store and checks the seed stored there: see [NATS seeds](#nats-seeds).

This version has one component, `envoy-infra:host:Listener`: an `envoy-listener` container on a
Docker host. It does not run NATS. The listener creates the `ENVOY_NOTIFICATIONS` stream and its
key-value buckets itself when it starts, so it needs only a NATS server with JetStream to reach.

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
program imports `github.com/sjawhar/envoy/infra/sdk/go/envoyinfra/host`.

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

## Providers

The listener creates its resources only with providers you pass it in its `providers` resource
option: a `docker` provider always, and a `command` provider too when `watchdog` is set. Its
resources use that provider at the version and with the configuration you give it, and the
provider's `host` decides which Docker daemon the listener runs on. The plugin never falls back to a
default provider of its own. Without one, the listener fails before it registers anything:

```text
envoy-infra:host:Listener needs a "docker" provider in its providers option
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

## Examples

The examples use placeholders: `example-host-a`, the `*.internal.example` hosts and each `<...>`
stand for your own values.

### YAML

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

### TypeScript

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
4. Put the public key into the NATS server's authorization as that user's `NKey`, with `HasSeed`
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
server's configuration is rendered from it, so it holds public keys only. This version's one
component, `host:Listener`, runs no NATS server and takes none.

| Field | What it is |
| --- | --- |
| `Users[].Name` | The user's name: lowercase letters, digits and hyphens, 1 to 48 characters, starting and ending with a letter or digit. |
| `Users[].NKey` | The user's public key, `U` and 55 more characters, as `envoy-infra seed generate` prints it. Empty for a pending user. |
| `Users[].Publish`, `Users[].Subscribe` | Each an `Allow` list and an optional `Deny` list of subject patterns. A subscribe allow entry may instead be `<subject> <queue>`, which scopes it to that queue group. |
| `Users[].HasSeed` | The consumer stores a seed for this user. |
| `NoAuthUser` | Optional: the name of a keyed user without a seed, whom a client presenting no credential becomes. |

A user with no `NKey` is pending: it stands for a seed whose key the server is not told yet, such
as one whose secret is declared but not yet written. It needs `HasSeed`, takes no rules and is not
rendered.

An authorization is refused, with an error naming the field and, for a user's field, the user as
`users[<index>] "<name>"`, when it has:

- a name, an `Allow` or `Deny` entry (a queue group included) or a `NoAuthUser` that holds an nkey
  seed, in any spelling listed under "What `check` and `verify` print". This is checked first, and
  the error names the field by its path and never the value, as `users[0] "example-listener":
  publish.allow[1] holds an nkey seed (not echoed)`, or `users[0]: name holds …` for a name: every
  other refusal quotes a name or an entry, and the block writes them;
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
- a `<subject> <queue>` entry in `Subscribe.Deny`, which nats-server before 2.15 enforces more
  loosely than it reads: beside a plain deny of the same subject it re-allows every other queue
  group, and alone it misses a wildcard subscription that covers its subject. Deny the plain
  subject, or narrow `Subscribe.Allow`;
- a `NoAuthUser` that names no user, a pending user or a user with `HasSeed`.

`Render` writes the header lines it is given, each as a `# ` comment, which may not hold a line
break or an nkey seed. Then comes the block: a comment naming each keyed user, in order, and
the user as one JSON object, with keys sorted, non-ASCII characters as they are, and `deny` only
when it names an entry. With a `NoAuthUser`, `no_auth_user` follows it, naming that user's key.
For three keyed users, a pending `example-pending` and `NoAuthUser` `example-fallback`, `core`'s
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
