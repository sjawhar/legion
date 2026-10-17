# `envoy-infra:gcp:NatsHub`

NATS with JetStream on one Google Cloud VM that joins a tailnet, where its clients reach it, as
the NATS server `envoy-infra:host:Listener` connects to. How to install the plugin, give a
component its providers, and write a host's NATS seed is in [the module's README](../README.md).

## What `gcp:NatsHub` deploys

A hub whose `hostname` is `<h>` registers these resources, each a child of the hub and named after
`<h>` rather than after the hub's own name:

| Resource | Type | What it is |
| --- | --- | --- |
| `<h>-hub` | `gcp:serviceaccount/account:Account` | The VM's service account, account id `<h>-hub`. |
| `<h>-tailnet-auth-key` | `gcp:secretmanager/secret:Secret` | The secret `<h>-tailnet-auth-key`, replicated automatically. |
| `<h>-tailnet-auth-key` | `gcp:secretmanager/secretVersion:SecretVersion` | Its version, holding `tailnetAuthKey`. |
| `<h>-tailnet-auth-key-accessor` | `gcp:secretmanager/secretIamMember:SecretIamMember` | The service account's grant of `roles/secretmanager.secretAccessor` on that secret. |
| `<h>-listener-seed-accessor` | `gcp:secretmanager/secretIamMember:SecretIamMember` | Only with `listener`: the same grant on `listener.seedSecret`, a secret the consumer declares and writes. |
| `<h>-network` | `gcp:compute/network:Network` | Only without `network`: an auto-mode network, with a subnetwork in every region. |
| `<h>-health-checks` | `gcp:compute/firewall:Firewall` | The hub's one firewall rule: tcp/8222 from Google's health-check ranges, `130.211.0.0/22` and `35.191.0.0/16`, to the VM's network tag `<h>-hub`. |
| `<h>-healthz` | `gcp:compute/healthCheck:HealthCheck` | An HTTP check of NATS's `/healthz` on port 8222, every 10 seconds, unhealthy after 6 failures. |
| `<h>` | `gcp:compute/instanceTemplate:InstanceTemplate` | The VM: `machineType`, a boot disk from Container-Optimized OS's `cos-stable` image family, the data disk, an external address for its way out to the image registries and the tailnet (see [The VM's external address](#the-vms-external-address)), the service account with the `cloud-platform` scope, the tag `<h>-hub`, and the cloud-config as its `user-data` (see [What the hub's VM runs](#what-the-hubs-vm-runs)). |
| `<h>` | `gcp:compute/instanceGroupManager:InstanceGroupManager` | The zonal group of that one VM, in `zone`. It keeps the data disk (see [The hub's data disk](#the-hubs-data-disk)), recreates the VM once the health check finds it unhealthy, from an initial delay after the VM is created that covers the first boot's read of the data disk (see [The first boot's read](#the-first-boots-read)), and replaces it when the template changes (see [Updating a hub](#updating-a-hub)). It is created after the key's version and the account's grants, since its first VM fetches the key when it boots and its listener fetches its seed when it starts, and `pulumi up` waits until it is `STABLE`. |

Every resource is in the gcp provider's project. The template's name is `<h>-` and a suffix the
provider adds, and the network, the firewall rule, the health check and the group are named after
their resource names with a random suffix, such as `<h>-healthz-292ee38`. The account id and the
secret id are exact, so two hubs in one project need two hostnames.

The hub's outputs are `natsUrl`, `nats://<h>:4222`, where its clients reach it over the tailnet,
and `instanceGroup`, the group's name.

## The hub's inputs

| Input | Default | What it is |
| --- | --- | --- |
| `users` | none | The NATS server's nkey users. See [The hub's users](#the-hubs-users). At least one is required unless the hub has a `listener`. |
| `tailnetAuthKey` | required | A secret: the Tailscale auth key the VM joins the tailnet with. Use a single-use, pre-approved key: the hub's service account can read it for as long as the hub exists, and so can root on the VM, the NATS container included, until the node has joined, so a reusable key would let either add devices with the hub's tags. The tailnet must let it advertise `tags`. May come from another resource or stack. |
| `hostname` | required | The VM's name on the tailnet and `natsUrl`'s host: 2 to 26 lowercase letters, digits and hyphens, starting with a letter and not ending with a hyphen (`^[a-z][a-z0-9-]{0,24}[a-z0-9]$`). It also names every child. |
| `tags` | required | The tailnet tags the VM advertises when it joins, each `tag:<name>`. At least one. |
| `zone` | required | The zone of the VM and its data disk, such as `us-central1-a`. Changing it makes a new group, with a new data disk: see [Updating a hub](#updating-a-hub). |
| `machineType` | `e2-small` | The VM's machine type. |
| `diskType` | `pd-balanced` | The type of the data disk the group creates for the hub's first VM: `pd-balanced` or `pd-ssd`, the types whose documented read rates the first boot's read of the disk is timed by (see [The first boot's read](#the-first-boots-read)). |
| `diskSizeGb` | `20` | The size, in GB, of that disk: at most 252 for `pd-balanced` and 506 for `pd-ssd`, the largest whose first-boot read fits the group's initial delay. |
| `natsImage` | required | The NATS server image, pinned by digest: `<name>@sha256:<64 lowercase hex digits>`. Its entrypoint takes nats-server's arguments, as the official `nats` image's does. |
| `tailscaleImage` | required | Tailscale's image, pinned the same way. The unit configures it through containerboot's `TS_*` variables, as Tailscale's own image reads them. |
| `network` | created | The name or self link of the VPC network the VM and the firewall rule are in. Unset, the hub creates one, which has no firewall rule but the hub's. The VM has an external address and takes the network's rules for all its instances, so on a network whose firewall admits SSH from anywhere, as the default network's `default-allow-ssh` does, its sshd is reachable from the internet: see [The VM's external address](#the-vms-external-address). May come from another resource or stack. |
| `subnetwork` | unset | The VM's subnetwork, for a network with custom subnetworks: one in `zone`'s region. Only with `network`. May come from another resource or stack. |
| `listener` | unset | The hub's own Envoy listener: `image`, the listener's image pinned as the images above are; `seedSecret`, the id of a Secret Manager secret in the hub's project holding its NATS user's seed; and `nkey`, that seed's public key. See [The hub's listener](#the-hubs-listener). |
| `sessionUser` | unset | `name` and `nkey`, the NATS user a client presenting no credential becomes, granted `SessionPermissions`. See [The hub's users](#the-hubs-users). |

Only `tailnetAuthKey`, `network` and `subnetwork` may come from another resource's output. Every
other input takes a value the program knows when it runs, and the plugin refuses an output there,
naming the input. So the cloud-config is known at preview.

The hub refuses these before it registers anything, with an error naming the input:

- an nkey seed, found as `envoy-infra seed` finds one (`bus.HoldsSeed`: any type, either case,
  whole, inside a longer value, split by `.`, `-`, `_` or whitespace, or percent-encoded), in the
  hub's own name or an input it carries into the user data, a child or a refusal: `hostname`, a
  tag, `zone`, `machineType`, `diskType`, an image, a user's `machineId`, or the listener's
  `image` or `seedSecret`. The error names the input by its path and never its value, as
  `users[0].machineId: an nkey user seed, which is never an input of the hub (not echoed)`, and a
  seed in the hub's own name as `envoy-infra:gcp:NatsHub: the component's name holds an nkey seed
  (not echoed)`. `network`, `subnetwork` and `tailnetAuthKey` may come from another resource, so a
  seed in any of them fails the preview or update once it resolves, with the same error, before
  any child is registered with it: a NATS seed passed as the Tailscale key is refused before a
  Secret Manager version holds it, and the error never carries the secret. A seed in a user's
  `name` or a rule entry is refused by the authorization, as [The hub's users](#the-hubs-users)
  says, and a seed as a user's `nkey` is refused as an `nkey` that is no public key, without
  quoting it;
- a `hostname` outside the rule above;
- no `tags`, or a tag that is not `tag:` followed by a letter and then letters, digits and
  hyphens, Tailscale's rule for a tag;
- an empty `zone`;
- an image that is not `<name>@sha256:<64 lowercase hex digits>`, with only letters, digits and
  `._/:-` in `<name>`: whitespace, `$` or `%` would split or expand the unit's command;
- an empty `machineType` or `diskType`, or a `diskSizeGb` below 1;
- a `diskType` other than `pd-balanced` or `pd-ssd`: `pd-standard` reads 0.12 MiB/s per GiB with
  no baseline, so its first-boot read takes over 8,500 s at any size, past the most the group
  waits, and an E2 VM, as the default `e2-small` is, attaches `hyperdisk-balanced` only by
  arrangement with Google's account team;
- a `diskSizeGb` above the largest for its `diskType`, with an error naming that size, as
  `diskSizeGb 253 is too large for a pd-balanced data disk: … the largest diskSizeGb that fits is
  252`;
- no `tailnetAuthKey`;
- `subnetwork` without `network`, since the firewall rule needs the network;
- no `users` and no `listener`, or a user refused as [The hub's users](#the-hubs-users) lists;
- a `listener` whose `image` is not pinned as above, whose `seedSecret` is not a secret id
  (`^[A-Za-z0-9_-]{1,255}$`, Secret Manager's rule, so a resource path such as
  `projects/<p>/secrets/<s>` is refused) or is the hub's own `<h>-tailnet-auth-key`, or whose
  `nkey` is empty;
- a `sessionUser` whose `nkey` is empty;
- a missing provider (see [Providers](../README.md#providers)).

## The hub's users

`users` holds the NATS server's nkey users, and `listener` and `sessionUser` add one each. Each of
`users` is `name`, `nkey` and exactly one of:

- `machineId`: the user of one host, whose listener and sessions all connect as it, granted
  `HostPermissions(machineId)` (see [Permission sets](../README.md#permission-sets)). Give it the
  listener's own `machineId`: the set names that machine's durable consumer and queue group.
- `permissions`: `{publish, subscribe}`, each a `nats:Rules`, an `allow` list and an optional
  `deny` list of subject patterns, for any other client.

`nats:Rules` is the one rules type every NATS component takes, whatever its cloud, and `nkey` is
named as `aws:NatsService`'s users name it, so a user is written alike for both components.

`nkey` is the public key `envoy-infra seed generate` prints, and the seed stays in the secret
store it wrote (see [NATS seeds](../README.md#nats-seeds)): the host's listener reads it from
there into its `seed`, and its `natsUrls` is the hub's `natsUrl`. In the authorization `core`
renders (see [NATS authorization](../README.md#nats-authorization)), each of `users` is a keyed
user with `HasSeed` set. The listener's user follows them, named `<h>`, with `listener.nkey` and
`ListenerPermissions(<h>)` and `HasSeed` set. `sessionUser` comes last, with
`SessionPermissions()` and no seed, as the authorization's `noAuthUser`: a client that presents no
credential becomes that user, and one that presents a key the server does not know is refused.
Without `sessionUser`, no user stands for a client presenting no credential, so the server refuses
every client without the seed of one of its users. Give `sessionUser.nkey` a public key whose seed
nobody keeps (`nk -gen user -pubout`, keeping only the `U…` line): no client presents it, and a
client holding its seed would only be the same session user. The rendered file, headed
`# Rendered by envoy-infra. Public keys only.`, is `/etc/nats/nats-server.conf` on the VM; the
VM's metadata holds it in the clear.

A session user lets anything that reaches 4222 publish and subscribe as `SessionPermissions()`
allows: it reads every notification topic, answers any reply inbox it sees there (a role
delivery's receipt included), and publishes agent stream frames and answers stream replay requests
for any session id, so it can fake any agent's live stream in Dispatch. With one, the tailnet's
policy is the gate on 4222: which devices it lets reach the hub's 4222 decides who is on the bus.
A `listener` opens a second door, port 9020, with or without a session user, and it grants far
more ([The hub's listener](#the-hubs-listener)).

A user is refused when it names both `machineId` and `permissions` or neither, or when its
`machineId` or `nkey` is empty, named by its index alone, as `users[1]: nkey is required`, since
these come before the authorization has checked the names. It is refused too for each of the
authorization's own refusals under [NATS authorization](../README.md#nats-authorization), named as
`users[<index>] "<name>"`: a name of another form or one two users share, an `nkey` that is not a
user public key (a seed included), or an empty `allow`. The authorization refuses a seed in a
user's name or a rule entry before any of those, naming the path and not the value, as
`users[1]: name holds an nkey seed (not echoed)` or
`users[1] "example-operator": publish.allow[0] holds an nkey seed (not echoed)`. The listener's
user and the session user are named by their inputs in the same refusals, as
`listener "<h>": nkey repeats users[0] "example-host-a"` or
`sessionUser "<name>": nkey is not a user public key …`; a user of `users` named `<h>` is refused
as the listener's user repeating it, as `listener "<h>": name repeats users[<index>]`.

## The hub's listener

With `listener`, the VM runs Envoy's listener beside NATS, as machine `<h>`. It listens on the
VM's loopback, `127.0.0.1:9020`, and reaches NATS there too, on `nats://127.0.0.1:4222`. Tailscale
Serve forwards the tailnet's connections to the hub's port 9020 to that address. A self-subscribed
client such as pi-envoy or claude-envoy registers and sends through
`http://<h>.<tailnet>:9020`; it receives directly from NATS, with no listener of its own. A client
that registers a serving port, including the OpenCode plugin, cannot receive delivery through this
hub listener: its registration is the hub's machine, and the hub has no host bridge to reach the
client's port. Run a listener beside that client instead. The listener's `/v1` API takes no bearer,
which the listener allows only on a loopback address (`packages/envoy/cmd/listener/apiauth.go`), so
the tailnet's policy for port 9020 is the only thing that decides who may call it, and a caller
gets the whole API with no credential: it can publish to any topic outside the agent topics with
a `source` of its choosing, under the listener's user's publish grant (Dispatch, GitHub, Slack,
Legion and role notifications included), claim a role, list every session and delete one. That
holds whether or not the hub has a `sessionUser`, so a hub without one still has an open bus for
any device the policy lets reach 9020: grant 9020 only to devices that may act as any agent.

Because the API takes no bearer, a web page open on any device the policy lets reach 9020 could
otherwise post to it: a browser sends a cross-site form without asking. The listener refuses any
request body whose `Content-Type` is not `application/json`, which a page can send cross-site only
after a CORS preflight the listener never grants (`apiauth.go`). That refusal shipped in
legion-envoy-v13.9.0, so pin `listener.image` to that release or a later one; an older image
leaves 9020 open to any page a tailnet user visits. The refusal does not stop DNS rebinding: a
page whose own name an attacker repoints at the hub's tailnet address is same-origin with it, so
it sends JSON with no preflight, reads the replies, and has the whole API, since the listener does
not check the request's `Host`. A browser that asks before a public page reaches a private or
tailnet address (Chrome's Local Network Access) blocks that; any other browser on a device the
policy admits to 9020 is a way in, which is one more reason to grant 9020 narrowly.

The listener connects as the listener's user (see [The hub's users](#the-hubs-users)) with the
seed `listener.seedSecret` holds. The hub's service account gets
`roles/secretmanager.secretAccessor` on that secret and nothing more; the consumer declares the
secret and writes the seed with `envoy-infra seed generate`, whose printed key is `listener.nkey`
(see [Making a seed](../README.md#making-a-seed)). The seed reaches the listener's environment as
`NATS_NKEY_SEED` through `docker run --env-file`, which Docker reads as root, since the listener's
image runs as an unprivileged user.

## What the hub's VM runs

The VM boots Container-Optimized OS (COS), which mounts `/` read-only and keeps `/etc` and
`/mnt/disks` on a tmpfs that does not outlive a boot
([COS file system](https://cloud.google.com/container-optimized-os/docs/concepts/disks-and-filesystem)),
so cloud-init applies the hub's cloud-config at every boot. The whole document, for the hub of
[A NATS hub, in YAML](#a-nats-hub-in-yaml), is `gcp/testdata/cloud-config.golden` in this module.

`bootcmd` runs first, in this order:

1. It loads the `tun` module unless `/dev/net/tun` exists, and makes the mount point,
   `/mnt/disks/data`.
2. It mounts the data disk, `/dev/disk/by-id/google-data`, formatting it first only when it is
   proven empty end to end: `blkid -p` finds no filesystem on it (exit status 2), and GNU `cmp`,
   which COS ships, reads the whole device, as many bytes as `blockdev --getsize64` reports,
   against `/dev/zero` and finds only zeros. cmp stops at the first byte that is not zero. A disk
   blkid finds a filesystem on is not read: it is checked with `fsck.ext4 -p` and mounted when fsck
   left no error it could not fix (exit status below 4). Every other disk is left unmounted and
   unwritten. Among them is an ext4 disk whose first MiB or primary superblock was zeroed: blkid
   finds no filesystem on it, yet it still holds its files' blocks, which `mkfs.ext4`'s discard
   would destroy, and a damaged superblock alone `e2fsck` repairs from a backup.
3. A disk left unmounted gets one line, at every boot that leaves it, naming the device and the
   reason and nothing the disk holds, as
   `envoy-hub: data disk /dev/disk/by-id/google-data left unmounted: blkid finds no filesystem on it, and it does not read as zeros to its end; see docs/nats-hub.md, "Repairing the data disk"`.
   The other reasons are `blkid -p exited <n>`, `blockdev --getsize64 exited <n>` or
   `cmp exited <n> reading it` after blkid found no filesystem, `mkfs.ext4 exited <n>`,
   `fsck.ext4 -p exited <n>` and `mount exited <n>`. An unmounted disk keeps NATS and Tailscale
   down, since their units refuse to start without it, so the group recreates the VM on the same
   disk, again and again, and no boot formats it: see
   [Repairing the data disk](#repairing-the-data-disk).
4. It makes `nats/` and `tailscale/` under the mount point, `/run/envoy`, mode 0700, and an empty
   `/run/envoy/tailnet-auth-key`, mode 0600, so the key's bind mount into the Tailscale container
   is always of a file.

`write_files` then writes the authorization to `/etc/nats/nats-server.conf`, the secret fetcher to
`/etc/envoy/fetch-secret`, the joined script to `/etc/envoy/tailnet-joined`, and Tailscale Serve's
configuration to `/etc/tailscale-serve/serve.json`. With a `listener`, it also writes the seed
script to `/etc/envoy/listener-seed-env`, and the Serve configuration forwards port 9020; without
one, the configuration is `{}`, which clears a forward from a previous VM. It then writes the
systemd units to `/etc/systemd/system/`, and `runcmd` reloads systemd and starts the units in this
order. They are started and never enabled, since `/etc` does not outlive the boot.

| Unit | What it does |
| --- | --- |
| `fetch-tailnet-key.service` | A oneshot, after `network-online.target`, that stays active once it exits: `/etc/envoy/fetch-secret /run/envoy/tailnet-auth-key <h>-tailnet-auth-key`. It runs only on a mounted data disk without `/mnt/disks/data/tailnet-joined`, so no boot after the node has joined fetches the key. |
| `tailscaled.service` | `docker run --name tailscaled` of `tailscaleImage` on the host's network, with `NET_ADMIN` and `/dev/net/tun`, its state in `/mnt/disks/data/tailscale`, the key file mounted read-only, `TS_AUTHKEY=file:/run/envoy/tailnet-auth-key`, `TS_AUTH_ONCE=true`, kernel networking (`TS_USERSPACE=false`), `TS_HOSTNAME=<h>` and `--advertise-tags` with `tags`. It mounts `/etc/tailscale-serve` read-only and sets `TS_SERVE_CONFIG=/etc/tailscale-serve/serve.json`: containerboot applies that configuration once the node is up, and again whenever the file changes, watching its directory (tailscale v1.104.1 `cmd/containerboot/serve.go`). With a `listener`, the configuration forwards port 9020; without one it is `{}`, which clears a prior forward. It wants the fetch and starts after it ends, but does not require it. |
| `tailnet-joined.service` | Started after `tailscaled`, on a mounted data disk without the marker: `/etc/envoy/tailnet-joined /mnt/disks/data/tailnet-joined /run/envoy/tailnet-auth-key`. Every 5 seconds the script asks the Tailscale container's CLI for `status --json`, and once its `BackendState` is `Running` it empties the key file, which stays for the container's bind mount, and only once that has succeeded writes the marker. A simple service, so its start returns at once; a run that fails, as one whose key drop fails does, is run again 10 seconds later. |
| `nats-firewall.service` | A oneshot that stays active once it exits: it adds `INPUT -p tcp -s 130.211.0.0/22,35.191.0.0/16 --dport 8222 -j ACCEPT` and `INPUT -i tailscale0 -p tcp --dport 4222 -j ACCEPT` to the host's iptables, each unless it is there. COS's host firewall accepts incoming connections for SSH alone ([COS firewall](https://cloud.google.com/container-optimized-os/docs/how-to/firewall)), so without them the health check never reaches NATS, and the tailnet reaches NATS only through the accept rule Tailscale's image writes for `tailscale0`, which a host filtering with another iptables backend overrides. |
| `nats.service` | `docker run --name nats` of `natsImage` on the host's network, with `/mnt/disks/data/nats` as `/data` and the configuration file mounted read-only, and the arguments `-c /etc/nats/nats-server.conf -js --store_dir /data -m 8222 --server_name <h>`. It is stopped with `docker stop -t 30`. It wants `nats-firewall` and starts after it. |
| `envoy-listener.service` | Only with a `listener`. Before each start, `/etc/envoy/listener-seed-env /run/envoy/listener.env <seedSecret>` writes the seed to that env file; then `docker run --name envoy-listener` of `listener.image` on the host's network, with `--env-file /run/envoy/listener.env`, `PORT=9020`, `ENVOY_LISTEN_HOST=127.0.0.1`, `ENVOY_MACHINE_ID=<h>` and `NATS_URLS=nats://127.0.0.1:4222`. `TimeoutStartSec=600` exceeds the fetcher's 590-second maximum: six attempts, each with three 30-second curl limits, separated by five 10-second waits. A seed that cannot be fetched fails the start, and the restart fetches again. It is stopped with `docker stop -t 30`, since the listener's own shutdown takes up to about 20 seconds. It wants `nats` and starts after it. |

The container units require `docker.service`, refuse to start unless `/mnt/disks/data` is a mount
point, so that their state never lands on the tmpfs beneath, and restart 10 seconds after their
container exits, or after their start fails.

The secret fetcher runs under `/bin/sh` with curl, jq and base64. It takes the VM's project id and
an access token for its service account from the metadata server, reads the secret's `latest`
version through Secret Manager's REST API, and moves the decoded value onto the file it is given
from a temporary file readable by root alone, only when the value is not empty. A new grant can
take a while to reach Secret Manager, so it tries six times, ten seconds apart, and exits 1 after
the sixth failure. Nothing of the value or the token is printed or passed in a command's
arguments. `GCE_METADATA_HOST` and `SECRET_MANAGER_ENDPOINT` replace the metadata server's host
and Secret Manager's base URL, which its tests use. The seed script fetches the listener's seed
with it into a temporary file, refuses a seed of more than one line, which would add a variable of
the secret's choosing to the listener's environment, and writes `NATS_NKEY_SEED=<seed>` to
`/run/envoy/listener.env`, readable by root alone, removing the fetched file.

NATS listens for clients on 4222 and serves its monitoring endpoints on 8222, on every interface.
The hub's VPC firewall rule and `nats-firewall` admit Google's health checks to 8222, and
`nats-firewall` admits the tailnet's interface, `tailscale0`, to 4222; nothing else is opened. So
the tailnet's policy decides which peers reach 4222. 8222's `/varz`, `/connz` and `/jsz` are
reachable over the tailnet only through Tailscale's own accept rule for `tailscale0`, as are its
other ports (see
[What a first Google Cloud deployment proves](#what-a-first-google-cloud-deployment-proves)). The
listener's port 9020 needs no host firewall rule: tailscaled takes a connection to a port Serve
publishes before the host's iptables sees it, and opens its own connection to `127.0.0.1:9020`.
On a Linux host with tailscaled 1.104.1 in kernel mode, a port Serve published still answered
over the tailnet while iptables dropped that port on `tailscale0`, and a port Serve did not
publish timed out. Serve publishes nothing else.

## The VM's external address

The template gives the VM an ephemeral external address, and the hub has no input that drops it.
The address is the VM's way out: to the registries `natsImage` and `tailscaleImage` are pulled
from, and to Tailscale's coordination and relay servers. A VM without one reaches the internet only
through [Cloud NAT](https://cloud.google.com/nat/docs/overview) on its subnetwork's region, which
a consumer's network need not have.

The address is also a way in. Besides the hub's own rule, the VM takes every ingress rule of
`network` that applies to all of the network's instances. The default network has four
([pre-populated rules](https://cloud.google.com/firewall/docs/firewalls)): `default-allow-ssh`
(tcp:22 from `0.0.0.0/0`), `default-allow-rdp`, `default-allow-icmp` and `default-allow-internal`
(every port from `10.128.0.0/9`). COS's host firewall then accepts SSH and nothing else that the
hub has not opened, so on such a network the VM's sshd is reachable from the internet. To avoid
that, leave `network` unset, so the hub creates a network holding only its own rule, or name a
network with no ingress rule from outside it that applies to every instance. The hub offers no
way to run with Cloud NAT and no external address: a consumer whose VMs must have no external
address can't use this version of `gcp:NatsHub`.

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
`cmd/containerboot/main.go:583-682`), so the VM logs in with `tailnetAuthKey` at its first boot
alone. Once the node runs, `tailnet-joined` empties the key file, then writes
`/mnt/disks/data/tailnet-joined`, and no later boot fetches the key: it is on the VM no longer
than the first login needs. A drop that fails writes no marker, so the unit tries again this boot
and every later boot fetches and drops the key again until a drop succeeds.
The marker is not tailscaled's own state file, which tailscaled writes before it logs in (tailscale
v1.104.1 `ipn/ipnlocal/local.go:3185-3190`), so a first boot whose login failed fetches the key
again. The same is why `tailscaled.service` does not require the fetch: on a later boot the key file
stays empty, and the node comes up from its state. A node removed from the tailnet therefore does
not log in again; to have it join anew, put a new key in `tailnetAuthKey`, run `pulumi up`, delete
`/mnt/disks/data/tailnet-joined` on the VM and restart it.

The group reattaches the same disk to every later VM, so a later `diskSizeGb` or `diskType`, which
changes only the template, should leave the disk at the size and type it was created with. Google
Cloud's documentation implies this without saying it, and a first deployment confirms it (see
[What a first Google Cloud deployment proves](#what-a-first-google-cloud-deployment-proves)).
Keep both at the disk's own values: the group's initial delay is derived from them. Deleting the
hub leaves the disk, detached, in `zone`, as the preserved-state page documents, and leaves the
hub's node on the tailnet; remove both yourself once you no longer need them.

### The first boot's read

A boot on a disk blkid finds no filesystem on reads the whole disk before it formats it: the
first boot does, on a new disk, and so does every boot on a disk it leaves unmounted. A boot on
the hub's own ext4 disk reads nothing. The group's initial delay, the time it waits after creating
a VM before the health check counts, is derived so that the read fits, in seconds:

```text
initialDelaySec = 300 + ceil(1.25 × (S + S/64 + 1024) / R)
```

`S` is the disk's size in MiB, `diskSizeGb` × 1024 (taking GB as GiB, the larger reading), and `R`
the read rate assumed for `diskType`, in MiB/s. 300 seconds cover the boot and the two image
pulls. `S` is the read, and `S/64` and 1024 MiB are what `mkfs.ext4` writes, its inode tables (256
bytes per 16 KiB, all written at once) and at most 1 GiB of journal. A quarter more is the margin.
`R` is 100 MiB/s for `pd-balanced`, below the 140 MiB/s baseline Google Cloud documents for it,
and 200 MiB/s for `pd-ssd`, its documented floor: an E2 shared-core VM, as `e2-small` is, reads a
persistent disk at 200 MiB/s at most, below `pd-ssd`'s 240 MiB/s baseline
([persistent disk performance](https://cloud.google.com/compute/docs/disks/performance)). The
default, 20 GiB of `pd-balanced`, waits 573 seconds. A group waits at most 3600 seconds, so the
largest disks are 252 GiB of `pd-balanced` (3589 seconds) and 506 GiB of `pd-ssd` (3596), and the
hub refuses a larger `diskSizeGb`. Once the delay has passed, the health check, every 10 seconds
and unhealthy after 6 failures, finds a VM still reading unhealthy within another minute, and
the group recreates it; the next boot reads from the start again. `pulumi up` waits for the group
to be stable, which a group is once its VMs are running and healthy
([MIG status](https://cloud.google.com/compute/docs/instance-groups/getting-info-about-migs)), so
the group's create and update timeouts are the initial delay after the provider's own 15 minutes
(pulumi-gcp v10.1.0's upstream `resource_compute_instance_group_manager.go:49-53`): 24 minutes 33
seconds by default.

On a Linux workstation, cmp itself reads far faster than those rates: GNU cmp 3.10, reading a loop
device over a sparse file against `/dev/zero` as the entry does, took 18.1 seconds for 20 GiB
(1133 MiB/s), 222.7 for 252 GiB (1158 MiB/s) and 458.2 for 506 GiB (1131 MiB/s). A sparse file's
holes are read from no disk, so these numbers are cmp's and the kernel's cost alone, on a faster
CPU than an e2-small's shared core, and say nothing of a persistent disk's rate, which only a
first deployment measures (see
[What a first Google Cloud deployment proves](#what-a-first-google-cloud-deployment-proves)).

## Repairing the data disk

A disk the boot leaves unmounted (see [What the hub's VM runs](#what-the-hubs-vm-runs)) keeps NATS
down, so the health check fails and, once the initial delay has passed, the group recreates the
VM on the same disk. Nothing writes a disk the boot refuses, so the next VM refuses it again, with
the same line, and the group recreates that one too: the loop goes on, and no boot formats the
disk, until the disk is repaired. To repair it, stop the group's repairs, keep a snapshot, repair
the disk on the hub's own VM, which keeps it attached, restart the VM, then undo each step.

The commands below follow gcloud 561.0.0's help for each and parse under it; none has been run
against a real group. `<group>` is the hub's `instanceGroup` output, `<zone>` its `zone` and `<h>`
its `hostname`.

1. Stop the group recreating the VM. The health check stays; the group no longer repairs a failed
   or unhealthy VM:

   ```sh
   gcloud compute instance-groups managed update <group> --zone <zone> --default-action-on-vm-failure=do-nothing
   ```

2. Find the VM, its data disk and its network, and read the line naming why the disk is unmounted
   (the data disk is the one whose `deviceName` is `data`; `<disk>` and `<network>` are the last
   part of its `source` and of the network's link):

   ```sh
   gcloud compute instance-groups managed list-instances <group> --zone <zone>
   gcloud compute instances describe <vm> --zone <zone> --format='yaml(disks[].deviceName,disks[].source,networkInterfaces[].network)'
   gcloud compute instances get-serial-port-output <vm> --zone <zone> | grep 'envoy-hub: data disk'
   ```

3. Keep a snapshot of the disk before anything writes it:

   ```sh
   gcloud compute disks snapshot <disk> --zone <zone> --snapshot-names=<h>-data-before-repair
   ```

4. Reach the VM. The network the hub creates admits no SSH, so add a rule for the range IAP's TCP
   forwarding connects from
   ([IAP TCP forwarding](https://cloud.google.com/iap/docs/using-tcp-forwarding)), for the VM's
   tag alone, and connect through IAP, which needs `roles/iap.tunnelResourceAccessor`:

   ```sh
   gcloud compute firewall-rules create <h>-repair-ssh --network <network> --allow tcp:22 --source-ranges 35.235.240.0/20 --target-tags <h>-hub
   gcloud compute ssh <vm> --zone <zone> --tunnel-through-iap
   ```

5. On the VM, repair the disk as its line says. For a filesystem whose primary superblock is
   damaged, check it from the first backup superblock, at block 32768 of the 4 KiB blocks
   `mkfs.ext4` makes on a disk of 1 GiB or more, and let e2fsck fix what it finds:

   ```sh
   sudo e2fsck -fy -b 32768 -B 4096 /dev/disk/by-id/google-data
   ```

   e2fsck exits 1 when it has fixed errors. On a 1 GiB loop disk whose superblock alone was
   zeroed, this brought back every file, byte for byte. A disk whose whole first MiB is gone has
   also lost the start of its inode tables, the root directory's among them: the same command
   makes it mountable but empty, every file gone (on the same loop disk it left none). Run it there
   only if you mean to give up what the disk held; until then the files' blocks are still in the
   snapshot from step 3, for a recovery tool to read. A disk that holds nothing worth keeping, such
   as one whose first format stopped partway, is formatted only by your decision:
   `sudo mkfs.ext4 -m 0 -E lazy_itable_init=0,lazy_journal_init=0,discard /dev/disk/by-id/google-data`.

6. Restart the VM, so that its boot finds the filesystem and mounts the disk, and check that the
   line is gone and the health check turns `HEALTHY`:

   ```sh
   gcloud compute instances reset <vm> --zone <zone>
   ```

7. Undo steps 4 and 1, and delete the snapshot once the hub is healthy and you no longer need it:

   ```sh
   gcloud compute firewall-rules delete <h>-repair-ssh
   gcloud compute instance-groups managed update <group> --zone <zone> --default-action-on-vm-failure=repair
   gcloud compute snapshots delete <h>-data-before-repair
   ```

Whether the line reaches the serial console, whether `do-nothing` stops the loop, and whether
e2fsck's repair, on a real disk, leaves what JetStream needs are for a first deployment to show
(see [What a first Google Cloud deployment proves](#what-a-first-google-cloud-deployment-proves)).

## Updating a hub

An instance template cannot change once it is created
([instance templates](https://cloud.google.com/compute/docs/instance-templates)), and the hub's
holds the cloud-config, so a change to anything the cloud-config is rendered from (`users`,
`listener`, `sessionUser`, `tags`, `natsImage`, `tailscaleImage`) or to `machineType`,
`diskType`, `diskSizeGb`, `network` or `subnetwork` makes a new template. So does a plugin release
that changes the rendering itself: the release that added `listener` and `sessionUser` renamed the
secret fetcher and gives every hub a Serve configuration, so upgrading to it replaces every
existing hub's VM once, with the outage below, even a hub that sets neither input. The group then
replaces its VM at once (`PROACTIVE`, minimal action `REPLACE`), deleting it before it creates the
next (`RECREATE`, no surge, at most one unavailable). NATS is down from the old VM's stop until the
new one's `nats` unit starts, and its clients reconnect then. The new VM gets the same data disk,
so it keeps the stream and rejoins the tailnet as the same node. Its boot disk comes from the
`cos-stable` family as it is then, so a new template also brings whichever COS release the family
names at that moment.

Three inputs work otherwise:

- `tags` reach the tailnet only when the node first logs in. containerboot runs `tailscale up`,
  with `--advertise-tags`, for a node that is not logged in, and otherwise `tailscale set`, which
  passes `--accept-dns=false` and `--hostname` and never `TS_EXTRA_ARGS` (tailscale v1.104.1
  `cmd/containerboot/main.go:583-682`, `cmd/containerboot/tailscaled.go:145-146` and `:169-185`).
  A change to `tags` still replaces the VM; change a joined node's tags on the tailnet itself.
- `hostname` names every child, so a new one is a new hub: Pulumi creates a new set of children,
  whose VM gets a new data disk, so a new tailnet node and an empty stream, then deletes the old
  children, all but the old disk; the old node stays on the tailnet. The new node logs in with
  `tailnetAuthKey`, which must then be a new single-use key: the hub's first login spent the last.
- `zone` replaces the group itself, whose zone cannot change (pulumi-gcp v10.1.0's upstream
  `resource_compute_instance_group_manager.go:120-126`). The group's name is generated, so Pulumi
  creates the new group, under a new name, before it deletes the old one. The new group's VM gets
  a new, empty data disk, so the stream starts empty, and logs in with `tailnetAuthKey`, which must
  then be a new single-use key. It asks for `<h>` while the old node still holds it, so
  Tailscale names it `<h>-1` ([machine names](https://tailscale.com/docs/concepts/machine-names)),
  and `natsUrl` names the old node, which goes offline when the old group is deleted. The old disk
  is left detached in the old zone. To move a hub to another zone:

  1. Set `zone`, and a new single-use `tailnetAuthKey`, and run `pulumi up`. It returns once
     it has deleted the old group, and with it the old VM.
  2. Remove the old node from the tailnet
     ([remove a device](https://tailscale.com/docs/features/access-control/device-management/how-to/remove)).
  3. Rename the new node from `<h>-1` to `<h>` (Edit machine name, on the machine names page
     above), which also renames it in MagicDNS.

  Until the rename, `natsUrl` names the old, offline node, so the hub's clients reconnect only
  after it, to the new VM's empty stream.

## What a first Google Cloud deployment proves

The unit tests run the hub under Pulumi's mocks, check its cloud-config against the golden, and
run the secret fetcher, the listener's seed script, the joined script and the data disk's entry
under `/bin/sh` against fakes and stubs; the seed command's Secret Manager store runs against a
fake too. The live tests run the listener as the listener's user and a session with no credential
as the session user on real NATS servers. None of them runs
Google Cloud, COS or a tailnet, so the first deployment is where these hold or fail. Watch them on
its first `pulumi up`:

- `/dev/net/tun` exists on COS without `modprobe`, so the Tailscale container starts.
- `docker exec tailscaled tailscale --socket=/tmp/tailscaled.sock status --json` answers on COS
  with Tailscale v1.104.1's image, and reports `"BackendState": "Running"` once the node has
  joined, so `tailnet-joined` empties the key file and writes its marker.
- The data disk is at `/dev/disk/by-id/google-data` when `bootcmd` runs, and a new persistent
  disk reads as zeros to its end, so the first boot formats it; `fsck.ext4` runs on COS as its
  documentation shows; and after `gcloud compute instance-groups managed recreate-instances` the
  new VM finds its filesystem (`blkid -p` exits 0), reads nothing and formats nothing, and keeps
  the stream and its tailnet node.
- How long the first boot's read of a real persistent disk takes, unmeasured: the initial delay
  assumes 100 MiB/s for `pd-balanced` and 200 MiB/s for `pd-ssd` (see
  [The first boot's read](#the-first-boots-read)). The first boot's serial output, or
  `journalctl -b` on the VM, shows when the read began and when `mkfs.ext4` ran.
- The line a disk left unmounted gets reaches the serial console and the journal, and the steps of
  [Repairing the data disk](#repairing-the-data-disk), none run against a real group:
  `--default-action-on-vm-failure=do-nothing` stops the group recreating the VM, and IAP's SSH
  reaches COS.
- Two statements this README takes from Google Cloud's documentation: a later `diskSizeGb` or
  `diskType` leaves the kept data disk at the size and type it was created with, and deleting the
  hub leaves that disk behind, detached (see [The hub's data disk](#the-hubs-data-disk)).
- The health check turns `HEALTHY` through COS's host firewall once `nats-firewall` has run, and
  `pulumi up`'s wait for `STABLE` ends within the provider's timeout despite autohealing's
  initial delay, 573 seconds for the default disk. Google Cloud accepts `maxSurgeFixed: 0` with
  `maxUnavailableFixed: 1` on a zonal group of one, and the template's `namePrefix`.
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
- containerboot applies `/etc/tailscale-serve/serve.json` once the node is up, so
  `tailscale serve status --json` on the VM's Tailscale container shows the forward of 9020, and
  `http://<h>.<tailnet>:9020/healthz` answers over the tailnet from a device the policy lets reach
  port 9020. That containerboot watches the file's directory was read in its source, not run.
- `envoy-listener.service`'s `ExecStartPre` failing, as it does until the seed's grant reaches
  Secret Manager, is restarted by `Restart=always` 10 seconds later. That was measured on systemd
  255, not on COS's own.
- `envoy-infra seed` against Secret Manager itself: that `versions/latest` names a newest version
  that is `DISABLED` or `DESTROYED`, that `GetSecretVersion` on `latest` answers `NOT_FOUND` for a
  secret with no version, that `ListSecretVersions` with no filter lists `DESTROYED` versions, and
  that `GetSecret` answers `expire_time` for a secret given a `ttl` or an `expireTime`. The store's
  lines under [What `check` and `verify` print](../README.md#what-check-and-verify-print) rest on
  these. Also that `AccessSecretVersion` on `versions/1` answers `NOT_FOUND` for a secret with no
  version and `FAILED_PRECONDITION` for a disabled or destroyed version 1, which `generate`'s check
  before the write rests on; that, after `generate`'s write, `AccessSecretVersion` on a `latest`
  that names no version yet answers `NOT_FOUND`; and how often the listing and `latest` show the
  write within `generate`'s pauses, 7.75 seconds in all. Google documents both as eventually
  consistent, and such operations as converging typically within minutes, but may take a few
  hours. A write they do not show in time ends as an error after writing, though `generate` read
  version 1 back by number; `generate` run again then refuses the secret, which holds version 1,
  and `check` and `verify` confirm the write once `latest` names it.

## A NATS hub, in YAML

The whole `Pulumi.yaml` of a project with one hub in the project `example-project`, reading the
tailnet auth key from a secret config value. `example-hub`, `example-project`, the
`example.invalid` images and each `<...>` are placeholders for your own values. Its users are
`example-host-a`, the host whose listener [the module's YAML example](../README.md#yaml)
runs, and `example-operator`, a client with permissions of its own. It runs a listener of its own,
whose seed is in the secret `example-hub-listener-seed`, and admits a client presenting no
credential as `example-sessions`. The users' public keys are the placeholders `core`'s tests use,
and the listener's and the session user's are placeholders too: put in the ones
`envoy-infra seed generate` printed for your users and your listener, and one whose seed you
discarded for the session user.

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
          nkey: UBERXOSHFHFLL7YEXXH7RYEJLK455TF4BOZLIHD5R6NF5FFISNHEXIZO
          machineId: example-host-a
        - name: example-operator
          nkey: UCYF4CJJNU55NFRALQXGPOAZRM5R5JBXI6OAZB2BHWJ4QTTB5XHFENDG
          permissions:
            publish:
              allow: [">"]
            subscribe:
              allow: [">"]
              deny: ["$SYS.>"]
      listener:
        image: example.invalid/envoy@sha256:00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff
        seedSecret: example-hub-listener-seed
        nkey: UBZEZDPQNFYMZ4J4XOLTB2IN5PBPZ6FZEGY55R623A4GPBPZLTQFKDYV
      sessionUser:
        name: example-sessions
        nkey: UCNPQVMRWOOJWCD7XDVLH6MEOVFBMTV4LAQSKZYROSETYJVFAWDMNKVA
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

The preview plans `+ 13 to create`: the stack, the gcp provider `gcp-example-project`, the hub
`example-hub`, and the hub's ten children as
[What `gcp:NatsHub` deploys](#what-gcpnatshub-deploys) lists them, the network and the listener's
seed grant among them. The secret `example-hub-listener-seed` must exist in `example-project`
before `pulumi up`, and hold the listener's seed before the VM starts its listener. Its
outputs are `natsUrl: "nats://example-hub:4222"` and `instanceGroup`, `example-hub-` and a random
suffix. The template's `user-data` is `gcp/testdata/cloud-config.golden` byte for byte. `pulumi up`
then applies it. The listener of the module's first example reaches this hub with `natsUrls` set
to `nats://example-hub:4222` and the seed of `example-host-a`.

