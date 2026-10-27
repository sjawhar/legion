# envoy-infra

Pulumi components that deploy Envoy, and a command for what they need outside Pulumi. The
components ship as an executable Pulumi plugin, `pulumi-resource-envoy-infra`, so a program in any
Pulumi language needs only the Pulumi CLI:

| Component | What it deploys |
| --- | --- |
| `envoy-infra:host:Listener` | An `envoy-listener` container on a Docker host: see [host:Listener](#hostlistener). |
| `envoy-infra:aws:NatsService` | A NATS server with JetStream on ECS Fargate, reachable in the VPC through Cloud Map and on a tailnet through a Tailscale sidecar: see [The AWS components](#the-aws-components). |
| `envoy-infra:aws:WebhookListener` | Envoy's webhook listener on ECS Fargate, behind your load balancer's target group, with an alarm on each log line that says a delivery or a start went wrong. |
| `envoy-infra:gcp:NatsHub` | A NATS server with JetStream on one Google Cloud VM that joins a tailnet, where its clients reach it: see [docs/nats-hub.md](docs/nats-hub.md), which says what it deploys, its inputs and how it is updated, with an example. |
| `envoy-infra:gcp:Dispatch` | Dispatch on GKE Autopilot, with Cloud SQL, served over a tailnet and, only to the namespaces `allowedNamespaces` names, inside its own cluster: see [gcp:Dispatch](#gcpdispatch). |

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
program imports `github.com/sjawhar/envoy/infra/sdk/go/envoyinfra/host`, `.../envoyinfra/aws` and
`.../envoyinfra/gcp`. A Python program imports `sjawhar_envoy_infra`, the generated
`sjawhar-envoy-infra` package.

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

## gcp:Dispatch

`envoy-infra:gcp:Dispatch` deploys one private Autopilot cluster, its VPC and Cloud NAT, a
private-IP Cloud SQL for PostgreSQL instance, and three Deployments: the app `<hostname>`, which
runs `envoy-dispatch` beside the Cloud SQL Auth Proxy native sidecar; the front door
`<hostname>-tailnet`, whose userspace Tailscale nodes host the Tailscale Service `svc:<hostname>`;
and `<hostname>-listener`, which runs `envoy-listener` alone. Tailscale Serve on the front door is
the tailnet ingress on port 443, and proxies to the app's ClusterIP Service on port 8766, which
also serves Dispatch's API in-cluster to the namespaces `allowedNamespaces` names; a NetworkPolicy
refuses every other namespace. The front door's SOCKS5 Service on port 1055 and the listener's
Service on port 9020 are reachable only from the workloads their own policies name: the app and
the listener may dial the proxy, and the app alone may dial the listener.

| Resource | Purpose |
| --- | --- |
| VPC, subnetwork, router and Cloud NAT | Private Autopilot nodes can pull images and reach the tailnet control plane without public node addresses. |
| Private services access range and connection | Cloud SQL receives a private IP on the component's VPC. |
| Autopilot cluster | Enables the Secret Manager add-on, Secret Sync with rotation (GKE 1.33 or later; a new Secret Manager version reaches a synced Kubernetes Secret whose SecretProviderClass reads `versions/latest` without an apply), and Workload Identity. |
| Cloud SQL instance, database, user and generated password | PostgreSQL 16, private IP only, automated backups and point-in-time recovery. |
| Kubernetes namespace (not with `existingNamespace`), ServiceAccount `<hostname>`, Secrets, ConfigMap and SecretProviderClass | The app's and the listener's account, which Workload Identity lets mount the NATS seed; the generated application secrets (`<hostname>-dispatch`), the listener's generated bearer in a Secret of its own (`<hostname>-listener`, one key `ENVOY_API_TOKEN`), the front door's auth key (`<hostname>-tailnet-auth-key`, one key `TS_AUTHKEY`), the Serve configuration (ConfigMap `<hostname>-serve`, rendered from the resolved `tailnetService`), and the NATS seed mount (and, with `azureEmbedKeySecret`, a second SecretProviderClass, `<hostname>-azure-embed-key`, for the Azure embedder's key). |
| ServiceAccount `<hostname>-tailnet`, Role and RoleBinding `<hostname>-tailnet-tls` | The front door's pods run as an account of their own, which the Role lets `get`, `update` and `patch` the one Secret `<hostname>-tailnet-tls` and nothing else (no `create`, since the component creates it), so neither the app nor the listener, nor the account `serviceAccountSubject` names, can write the certificate. |
| Secret `<hostname>-tailnet-tls` | The Tailscale Service's certificate, its key and the ACME account key (keys `tls.crt`, `tls.key` and `acme-account.key.pem`), which the front door's pods publish and seed from ([The front door](#the-front-door)). The component declares it with no data and ignores its contents, so a later preview never plans to undo what the pods wrote, and no pod template carries its checksum, so a renewed certificate rolls no pod. |
| Three ClusterIP Services and three NetworkPolicies | Dispatch's API on port 8766, to the `allowedNamespaces` and the front door; the front door's SOCKS5 proxy on port 1055, to the app and the listener; the listener's API on port 9020, to the app. Each policy admits those sources alone among pods (Kubernetes still admits each pod's own node). |
| Three Deployments | The app at `replicas` pods, the front door at `frontDoorReplicas` (2 by default) and the listener at `listenerReplicas` (1 by default), each `Recreate` at one pod and `RollingUpdate` (`maxUnavailable: 0`, `maxSurge: 1`) above one, by its own count. Every pod carries required pod anti-affinity on `kubernetes.io/hostname` and a `topology.kubernetes.io/zone` spread with `minDomains: 2`, both over its own Deployment's pods, and every serving container a startup and a liveness probe ([The pods](#the-pods)). The app and the listener have a 35-second termination grace period, a five-second pre-stop wait and the stop each is built for; the front door 60 seconds, for its drain ([The front door](#the-front-door)). Every container has equal requests and limits. A pod template whose Deployment has no PodDisruptionBudget carries `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"`; one whose Deployment has a budget does not. |
| Up to three PodDisruptionBudgets | `<hostname>` with `appPdbMinAvailable`, `<hostname>-tailnet` with `frontDoorPdbMinAvailable` and `<hostname>-listener` with `listenerPdbMinAvailable`, each over its own Deployment's pods (`app=<name>`) alone, with that `minAvailable` and Kubernetes' default `unhealthyPodEvictionPolicy`, so a drain cannot take that workload below that many ready pods. |

### Inputs

The required plain inputs are `region`, `hostname`, `tailnetDnsName`, `tags`, the four
digest-pinned images, `signInClientId`, and `signInDomain`. `tailnetAuthKey`, `tailnetService`,
`natsUrl`, `natsSeedSecret`, and `signInClientSecret` are Pulumi inputs and may come from another
resource or stack. `tailnetAuthKey` and `signInClientSecret` are secrets. `databaseVersion`
defaults to `POSTGRES_16`, `databaseTier` to `db-custom-4-15360`, `namespace` to `dispatch`, and
`deletionProtection` to `true`; it sets both Pulumi's own guard and the Cloud SQL instance's
`deletionProtectionEnabled`, which refuses a delete from the API or the console too, and the
`dispatch` database's `deletionPolicy` `PREVENT` (otherwise `DELETE`), which keeps an instance
replacement from deleting the database first.
`existingNamespace`, `false` by default, makes the component use
the namespace as it exists in the cluster rather than create it, for a deployment where another
program owns the namespace. On a stack that has already applied, turning it on removes the
Namespace from the program, and the next update deletes the namespace and everything in it, the
front door's certificate Secret included: first remove the Namespace from the stack's state
(`pulumi state delete <the Namespace's URN>`), so the update leaves it in the cluster. The flag is
for a cluster that already exists: on a first apply the component creates the cluster, so no other
program can have made the namespace yet. `allowedNamespaces`, empty by default, each a DNS label,
names the in-cluster namespaces the NetworkPolicy admits on port 8766.

`tailnetService`, required, is the Tailscale Service the front door's pods host, as Tailscale's
API names it. It must resolve to `svc:<hostname>`, the name the certificate and the URL carry, and
any other value fails the update once it resolves, naming the input and never echoing the value.
The component never creates the Service: the consumer declares it in the tailnet, with an
auto-approval for `tags` in the tailnet policy, and passes the `Name` output of its
`tailscale.Service` here. The Serve ConfigMap `<hostname>-serve` is rendered from the resolved
value, and the front door's Deployment waits for that ConfigMap and for whatever the value came
from, so on a fresh stack no pod advertises the Service before it exists: a node that advertises a
Service before it exists is not approved when the Service appears.

`frontDoorToolsImage`, required and pinned by digest like the other three images, is the image the
front door's `cert-publish` container runs on. Its scripts call `/bin/sh`, `curl`, `openssl` 3,
`base64`, GNU `date`, `sed`, `grep` and `tr`, so the image must carry them all; the envoy image
does, and the Tailscale image, which is busybox with neither `curl` nor `openssl`, does not. It is
an input of its own rather than `envoyImage` so a release of the app's image never rolls the front
door: pin the envoy image here at a digest that moves only on purpose. The container runs as uid
999, the envoy image's `envoy` user, which every container of a front-door pod shares, since
tailscaled writes its certificate cache readable by its own uid alone.

`databaseTier`, `db-custom-4-15360` (4 vCPU and 15 GB) by default, is sized by connections. Cloud
SQL's default `max_connections` follows the instance's memory, and from 15 to under 30 GB it is
500 ([Configure database flags](https://cloud.google.com/sql/docs/postgres/flags),
`max_connections`). Every app pod opens up to the sum of its pools
(`packages/envoy/internal/dispatch/store/pool.go`): 64 shared, on which migrations run too, 4 for
document rooms, 1 for health, 9 for leases, 1 for the outbox, and the 3 `LISTEN` connections the
listener pool dials and hands off, 82 in all; the listener's and the front door's pods open none.
Six pods open 492, inside 500 with 8 to spare, and a rolling update runs `replicas` plus one pod,
so up to 5 replicas roll inside it. The previous default, `db-custom-1-3840` (3.75 GB, 100
connections), holds one pod and no roll, and its one vCPU served every query. A deployment that
left `databaseTier` unset moves to the new default on its next update; one whose instance
runs another tier, set by hand, sets `databaseTier` to that tier so the update does not resize it.

`databaseAvailabilityType` is the Cloud SQL instance's availability type: `ZONAL`, the default and
the instance's value before the input existed, keeps it in one zone, and `REGIONAL` keeps a
standby in a second zone that Cloud SQL fails over to. Any other value is refused by name before
the component registers a resource. Switching changes the instance in place, since the bridged
Terraform provider declares `settings.availability_type` without ForceNew
(`terraform-provider-google-beta` v8.6.0, which pulumi-gcp v10.1.0 bridges,
`google-beta/services/sql/resource_sql_database_instance.go:449-462`): Cloud SQL restarts the
instance once, which is a brief database outage, and the change cannot be stopped once started.
`REGIONAL` roughly doubles the instance's cost, since Cloud SQL bills the standby's CPU, memory
and storage as well ([Cloud SQL pricing](https://cloud.google.com/sql/pricing)).

`replicas`, `frontDoorReplicas` and `listenerReplicas` are the pod counts of the app, the front
door and the listener: 1, 2 and 1 by default, each refused by name below 1 before the component
registers a resource. Each Deployment takes its update strategy from its own count ([The
pods](#the-pods)). The front door runs two by default, since with one pod every node drain,
upgrade or scale-down that moves it takes Dispatch off the tailnet until its replacement is ready.
Raise `replicas` above 1 only on an envoy image that meets "Running more than one process" in
`packages/envoy/cmd/dispatch/README.md` (below). Raise `listenerReplicas` above 1 only on an envoy
image whose listener holds a delivery lease: there a standby pod is ready and serves `/v1` and the
webhooks while only the lease holder delivers, a stopping holder hands the lease over in about a
second, and a killed holder's lease is taken over in about six.

`appCpu` and `appMemory` size the app Deployment's `envoy-dispatch` container, and `proxyCpu` and
`proxyMemory` its `cloud-sql-proxy` sidecar: plain Kubernetes quantities (`2`, `1500m`; `8Gi`,
`6144Mi`), each rendered as both its request and its limit. `appCpu` and `appMemory` are 1 CPU and
`4Gi` by default, the size the container always had. `proxyCpu` and `proxyMemory` are 1 CPU and
`2Gi` by default: every database call of the pod passes through the proxy's TLS, up to the 82
connections of one pod's pools (above), and the proxy's own Kubernetes sidecar example requests 1
CPU, since its CPU use scales with the traffic between the database and the application, and
`2Gi`, since its memory scales with the active connections
([Run the Cloud SQL Auth Proxy as a sidecar](https://github.com/GoogleCloudPlatform/cloud-sql-proxy/blob/v2.26.3/examples/k8s-sidecar/README.md)).
One server pod's pools are that example's load. With the defaults the app pod requests 2 CPU and
`6Gi`. The front door and the listener keep their own sizes. Autopilot sizes the pod by the two
containers' sum, so before it registers a resource the component refuses, by name, a value that
is not a quantity, is empty or is not above 0, a decimal exponent outside `e-9` to `e18` (the
range the suffixes `n` to `E` cover; write a suffix instead), and sizes at which Autopilot would
not run the pod as rendered: a pod below `500m` or `512Mi` (the floor of a pod with pod
anti-affinity, which every Dispatch pod carries, and of a `safe-to-evict: "false"` pod) or outside
1 to 6.5 GiB of memory per CPU, which Autopilot raises, and a pod above 30 CPU or `110Gi`, which
Autopilot rejects; these are the bounds of Autopilot's default general-purpose compute class
([Resource requests in Autopilot](https://cloud.google.com/kubernetes-engine/docs/concepts/autopilot-resource-requests)).
The pod runs at exactly the rendered size only on a cluster that supports Pod bursting; on one
that does not, Autopilot also rounds the pod's CPU up to a multiple of 250m.
A change to any of the four replaces the app's pods as any change to its pod template does:
recreated at one replica, rolled above one.

`pprofPort`, unset by default, turns on `envoy-dispatch`'s Go profiler: the component sets
`DISPATCH_PPROF_PORT` to it on the `envoy-dispatch` container alone, and Dispatch serves
`net/http/pprof` on `127.0.0.1` at that port inside the pod. No Service, NetworkPolicy or other
container changes, so the profiler is reached only through `kubectl port-forward <pod>
<port>:<port>`, then `go tool pprof -seconds 30 http://127.0.0.1:<port>/debug/pprof/profile`
(`packages/envoy/cmd/dispatch/README.md`, "Profiling"). Unset, nothing is rendered, so a
deployment that leaves it unset renders the pod it always had. Before it registers a resource the
component refuses, by name, a value outside 1-65535 and the two ports the app pod already binds on
its loopback, `envoy-dispatch`'s `8766` and the `cloud-sql-proxy` sidecar's `5432`. Setting or
unsetting it replaces the app's pods as any change to its pod template does.

`priorityClassName`, `appPdbMinAvailable`, `frontDoorPdbMinAvailable` and
`listenerPdbMinAvailable` protect the pods from being displaced; with none set the component
renders no priority class and no budget.

`priorityClassName` is the `priorityClassName` of every pod of the three Deployments: the name, a
DNS subdomain, of a PriorityClass that already exists in the cluster. The component never creates
the class, but it reads it once, and all three Deployments wait on that read: Kubernetes refuses
every pod that names a class the cluster does not have, and at one replica `Recreate` would
already have stopped the old pod, so a missing class fails the update, and the preview too once
the cluster exists (on a first deploy the cluster's provider is unknown at preview, so the read
waits for the update), instead of leaving a workload down. Create the class before, or outside,
the program that sets the name. The `system-` prefix is refused: Kubernetes reserves it for
`system-cluster-critical` and `system-node-critical`, which outrank every workload so the
cluster's own components are placed first. A pod's priority decides which pods the scheduler may
preempt to place it, when its class allows preemption, and, among pods that alike exceed (or
alike stay within) their requests, decides which a node under memory or disk pressure evicts
first. Like `appCpu`, a change to it replaces the pods, here of all three Deployments.

`appPdbMinAvailable`, from 1 to `replicas`, registers the PodDisruptionBudget `<hostname>` over
the app's pods; `frontDoorPdbMinAvailable`, from 1 to `frontDoorReplicas`, registers
`<hostname>-tailnet` over the front door's; and `listenerPdbMinAvailable`, from 1 to
`listenerReplicas`, registers `<hostname>-listener` over the listener's. Each selects its own
Deployment's `app` label alone and sets that `minAvailable`. A Deployment with a budget loses the
`safe-to-evict` annotation ([The pods](#the-pods)): the budget bounds every voluntary eviction,
the cluster autoscaler's scale-down among them, and lets the autoscaler move those pods as far as
the budget allows. It also makes a drain wait, a manual upgrade, a node repair or an auto-upgrade
among them, and at its Deployment's replica count it refuses every voluntary eviction. So at one
replica with a budget of 1, every drain of that pod's node waits: `kubectl drain` waits with no
limit (until its own `--timeout`, if one is given), and a GKE upgrade waits up to an hour (below).
Each keeps Kubernetes' default `unhealthyPodEvictionPolicy`, `IfHealthyBudget`, so it holds a pod
that is not ready too while the budget is not met: `envoy-dispatch` and `envoy-listener` report
not ready whenever NATS or Postgres is briefly out of reach (a front-door pod's own eviction in
the same drain is one way), and evicting one then would turn that into a cold restart. A
front-door pod is ready only while it hosts the Service ([The front door](#the-front-door)), so
the front door's budget counts only pods that serve. A drain of a pod that stays broken waits the
longer for it: GKE respects a budget for up to an hour while it drains a node for an upgrade and
then evicts anyway
([Autopilot cluster upgrades](https://cloud.google.com/kubernetes-engine/docs/concepts/cluster-upgrades-autopilot)).
A budget never slows its Deployment's own rollouts, which Kubernetes does not run through the
eviction API. Delete every budget made by hand over the component's pods (`kubectl -n <namespace>
delete pdb <name>`) just before the first update that sets the matching input, one already named
`<hostname>`, `<hostname>-tailnet` or `<hostname>-listener` included: the component's Kubernetes
provider does not adopt an object that already exists, so the update fails with the budget's
`AlreadyExists`, and a hand-made budget under another name would select the same pods, which makes
the eviction API refuse every eviction of them.

The component refuses, each by name, before it registers a resource, a `priorityClassName` that
is empty, not a DNS subdomain or has the `system-` prefix, and a budget below 1 or above its
Deployment's replica count.

Two optional groups each come as all their inputs or none. Neither takes a secret value: the
broker relay's credential is a token the kubelet mints and the App key is read at run time, so no
broker credential or App key ever enters the component or its Pulumi state. Both reach the app's
`envoy-dispatch` container alone; the front door and the listener are the same with them as
without.

`agentSecretsUrl` with `agentSecretsAudience` (both plain) turns on Dispatch's secrets-broker
relay: the URL must be absolute http(s) with a host and no path, as Dispatch requires of
`DISPATCH_AGENT_SECRETS_URL` (a trailing slash is trimmed, as Dispatch trims it), and is rendered
as that plain env var. Dispatch calls the broker as its own Kubernetes ServiceAccount,
`system:serviceaccount:<namespace>:<hostname>`, never with a bearer it shares with the broker: the
app pod gets a projected volume of its own, `agent-secrets-account-token`, whose one source is a
`serviceAccountToken` for the audience `agentSecretsAudience` names, with `expirationSeconds: 3600`
(the broker refuses a token living longer than an hour from its issue), written to the file
`agent-secrets-account-token` and mounted read-only into `envoy-dispatch` at
`/var/run/secrets/dispatch`; `DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE` names
`/var/run/secrets/dispatch/agent-secrets-account-token`. The kubelet rewrites that file in place
before the token expires and Dispatch reads it on every call, so nothing stores the token, no
rotation restarts a pod, and no Secret Manager secret, grant or SecretProviderClass exists for it.
The URL must be reachable from the app pod's own network, because Dispatch's broker client dials
it directly, never through the front door's SOCKS5 proxy (which only the NATS clients use): an
Internet host through Cloud NAT, an in-cluster Service, or a name a private DNS zone on the
cluster's VPC resolves to one. A name only the tailnet resolves fails at request time, not at
boot. The URL is rendered as a plain environment variable, so one carrying a user, a password, a
query or a fragment is refused, and no refusal quotes it.

The broker accepts that token only once its own configuration names it, so the broker's
deployment changes in the same deploy as this component's:

- `BROKER_K8S_OIDC_AUDIENCE` is the value given here as `agentSecretsAudience`;
- `BROKER_K8S_OIDC_ISSUER` is the issuer of this component's cluster, which GKE gives as
  `https://container.googleapis.com/v1/projects/<project>/locations/<region>/clusters/<hostname>`;
- `BROKER_API_ACCOUNTS` lists Dispatch's account, as
  `dispatch=system:serviceaccount:<namespace>:<hostname>`, in place of the `BROKER_UI_TOKEN` it
  once shared with Dispatch.

A broker started with `BROKER_UI_TOKEN` or `BROKER_UI_TOKEN_FILE` set refuses it by name, as
Dispatch refuses `DISPATCH_AGENT_SECRETS_TOKEN` and `DISPATCH_AGENT_SECRETS_TOKEN_FILE`, and a
Dispatch whose account the broker does not list is answered 403 `ACCOUNT_NOT_LISTED` on every
call, so move the component, the Envoy image and the broker's configuration together. A stack that
set the removed `agentSecretsTokenSecret` sets `agentSecretsAudience` in its place; its next update
deletes the SecretProviderClass `<hostname>-agent-secrets-token` and the grant
`<hostname>-agent-secrets-token-accessor`, and the Secret Manager secret that held the old bearer
is the consumer's to delete.

`appClientId`, `appId` (the App's numeric ID, as a string of digits), `appPrivateKeySecretName`
and `appPrivateKeySecretKey` (plain: the name and key of an existing Kubernetes Secret in the
component's namespace holding the App's PEM private key, as GKE SecretSync copies it from Secret
Manager, or the base64 of that PEM) turn on Dispatch's GitHub App: `DISPATCH_APP_CLIENT_ID` and
`DISPATCH_APP_ID` are plain env vars, and `DISPATCH_APP_PEM_B64` is a `secretKeyRef` to that Secret
and key. The component never creates that Secret and the Deployment does not wait on it: GKE
SecretSync creates it at run time from Secret Manager, so the pod starts once the Secret exists.
The key is never parsed here; Dispatch checks it at boot (an RSA private key, PKCS#1 or PKCS#8).
Setting the App also starts Dispatch's webhook redelivery sweep, which needs the NATS redelivery
bucket the hub's Dispatch user is granted (`DispatchPermissions`): a pod whose NATS user lacks it
exits at boot. With either group unset, nothing renders and the app's pod template is unchanged.

The Azure embedder's inputs are a third optional group, which takes no secret value either, and
which reaches the app's `envoy-dispatch` container alone. `azureEmbedEndpoint` turns meaning search
onto Azure AI Foundry: `DISPATCH_EMBEDDER=azure` and `DISPATCH_AZURE_EMBED_ENDPOINT`, the base URL
the embeddings route is appended to (`https://<resource>.services.ai.azure.com/models`). It comes
with `azureEmbedTokensPerMinute` (plain, a positive integer, the deployment's quota, rendered as
`DISPATCH_AZURE_EMBED_TOKENS_PER_MINUTE`), may come with `azureEmbedDeployment`
(`DISPATCH_AZURE_EMBED_DEPLOYMENT`; unset, Dispatch's default `embed-v-4-0`), and takes exactly one
credential form. The key form is `azureEmbedKeySecret`, a Secret Manager secret's resource path
holding the deployment's API key, read at run time as the NATS seed is: through the
SecretProviderClass `<hostname>-azure-embed-key` and its own CSI volume, mounted read-only at
`/var/run/secrets/azure-embed`, whose file `key` `DISPATCH_AZURE_EMBED_KEY_FILE` names, under a
grant of its own (`<hostname>-azure-embed-key-accessor`) the app Deployment waits for. The keyless
form is `azureTenantId` with `azureClientId`, an Azure managed identity's tenant and client ids,
rendered as `DISPATCH_AZURE_TENANT_ID` and `DISPATCH_AZURE_CLIENT_ID`: the kubelet projects a
service-account token for the audience `azureFederationAudience` (plain; unset,
`api://AzureADTokenExchange`), valid for an hour and rotated before it runs out, into the volume
`azure-federation` at `/var/run/secrets/azure-federation/token`, which
`DISPATCH_AZURE_FEDERATED_TOKEN_FILE` names, and Dispatch exchanges it with Microsoft Entra for an
access token on every renewal. The endpoint, the deployment, the key secret's path and the two ids
are Pulumi inputs and may be another resource's outputs (an Azure program's, for the endpoint and
the identity). Every replica gets the same values, which Dispatch requires: one deployment runs
one embedder configuration, and a model change on Azure takes a new `azureEmbedDeployment`, since
the vectors' identity names the deployment, not the model (`packages/envoy/cmd/dispatch/README.md`,
"Search"). Without `azureEmbedEndpoint`, Dispatch embeds on Bedrock as it always did, and nothing of
this group renders.

Every image must be pinned by SHA-256 digest. Verify an `envoyImage` digest from
`ghcr.io/sjawhar/legion/envoy` before pinning it, with the `gh attestation verify` command
[Running Dispatch](https://sjawhar.github.io/legion/dispatch/running-dispatch/) gives. The
component refuses invalid hostnames, tailnet DNS names, tags, image references, sign-in domains,
namespaces, allowed namespaces, an `appCpu`, `appMemory`, `proxyCpu` or `proxyMemory` outside
Autopilot's bounds for the app pod (above), a `pprofPort` outside 1-65535 or on a port the app pod
already binds (above), a `replicas`, `frontDoorReplicas` or `listenerReplicas` below 1, a
`priorityClassName` that is empty, not a DNS subdomain or a `system-` class, an
`appPdbMinAvailable`, `frontDoorPdbMinAvailable` or `listenerPdbMinAvailable` outside 1 to its
Deployment's replica count, a `databaseAvailabilityType` other than `ZONAL` or `REGIONAL`, missing
inputs, and nkey seeds in its own name and in plain inputs before it registers a resource; it
refuses an `agentSecretsUrl` or an `agentSecretsAudience` whose partner input is missing, an
`appClientId`, `appId`, `appPrivateKeySecretName` or `appPrivateKeySecretKey` set without the
others (each missing input named), an `appId` that is not digits or is 0, an
`appPrivateKeySecretName` that is not a DNS subdomain, an `appPrivateKeySecretKey` outside
`[-._a-zA-Z0-9]+`, and an `agentSecretsUrl` that is not an absolute http(s) URL with no path; and,
of the Azure group, any of its inputs without `azureEmbedEndpoint`, an endpoint without
`azureEmbedTokensPerMinute` or with one below 1, both credential forms or neither, `azureTenantId`
or `azureClientId` without the other, and an `azureFederationAudience` without them or empty.
Once each Pulumi input resolves, before any child takes it, it refuses a seed in
`tailnetAuthKey`, `signInClientSecret`, `natsSeedSecret`, `azureEmbedEndpoint`,
`azureEmbedDeployment`, `azureEmbedKeySecret`, `azureTenantId` and `azureClientId`; a blank or
whitespace-only `tailnetAuthKey` or `signInClientSecret`, neither of which can work blank (the
front door cannot join the tailnet, Dispatch refuses a blank sign-in secret at boot); a
`tailnetService` other than `svc:<hostname>` — each a refusal naming the input and never echoing
it; and a `natsUrl` that holds a seed, is local (the pods reach NATS only through the front door's
SOCKS5 proxy), is empty, or holds a comma or a control character, each a refusal naming
`natsUrl`. It requires both `gcp` and `random` providers. It creates its Kubernetes provider from
the cluster's stable exec-plugin kubeconfig, so the machine running Pulumi needs
`gke-gcloud-auth-plugin`.

The generated `principal` output is the Kubernetes service account's Workload Identity principal,
`principal://iam.googleapis.com/projects/<project number>/locations/global/workloadIdentityPools/<project id>.svc.id.goog/subject/ns/<namespace>/sa/<hostname>`,
built from the cluster's own Workload Identity pool, so it resolves only once the cluster that
creates the pool exists. The app's Cloud SQL Auth Proxy needs `roles/cloudsql.client` on the
project for that principal, which the component does not write, since writing a project IAM
member needs `resourcemanager.projects.setIamPolicy`: the consumer grants it from a program whose
identity holds that permission, building the member from the principal's known form rather than
from the `principal` output, which resolves only once every child has registered, the app
Deployment's readiness wait included, and that wait needs the grant already in place. Whether IAM
accepts that member before the project's first Workload Identity cluster creates its pool is
unproven (`docs/plans/2026-10-14-dispatch-gcp-plan.md`, the hardening ledger); if it refuses it,
the first apply stops at the app's readiness wait, and a grant made then lets the next apply
finish. The component itself grants that
principal `roles/secretmanager.secretAccessor` only on the NATS seed secret (and on the Azure key
secret when `azureEmbedKeySecret` is set), and the app and the listener, which mount the seed,
wait for that grant to be written, as the app waits for the Azure key's. IAM can take minutes to apply
a new binding (Google cites typically two, sometimes seven or more), and a pod restarts until it
can mount. The app and the listener share that one Kubernetes service account, which holds no
Role: no pod creates a Secret, since each front-door node keeps its tailnet state on its own pod's
`emptyDir`, and the front door's pods run as `<hostname>-tailnet`, whose Role lets it write only
the Secret `<hostname>-tailnet-tls`.

The `serviceAccountIssuer` and `serviceAccountSubject` outputs are the issuer and subject of the
app's and the listener's Kubernetes service-account tokens (the front door's pods run as an
account the subject never names): the cluster's own issuer,
`https://container.googleapis.com/v1/projects/<project id>/locations/<location>/clusters/<cluster name>`,
and `system:serviceaccount:<namespace>:<hostname>`. The keyless Azure form rests on them: the
managed identity `azureClientId` names holds a federated identity credential whose issuer is
`serviceAccountIssuer`, whose subject is `serviceAccountSubject` and whose audience is
`api://AzureADTokenExchange` (or the `azureFederationAudience` set), which admits these pods'
projected tokens and no other, and the identity needs a role on the Azure AI Foundry resource that
lets it call the deployment. The component writes nothing in Azure: the consumer's Azure program
declares the credential and the role from these two outputs.

### The pods

The front door's pods are the tailnet nodes ([The front door](#the-front-door)). Each serves
`https://<hostname>.<tailnetDnsName>` for the Tailscale Service and proxies it to the app's
Service, and offers tailnet egress as its own node through its userspace SOCKS5 proxy, which binds
`0.0.0.0:1055` since its callers are pods of their own rather than containers beside it. The app
and the listener reach the NATS hub at its tailnet name through that proxy's Service, and the
policy admits those two alone, since whatever reaches the proxy leaves the cluster as a front-door
node.

The app pod runs `envoy-dispatch` with the Cloud SQL Auth Proxy native sidecar, which listens only
on `127.0.0.1:5432` with `--private-ip`. The listener pod runs `envoy-listener` alone, bound to
`0.0.0.0:9020` rather than to loopback, so its API demands a bearer: the component generates one,
keeps it in the Secret `<hostname>-listener`, and gives the same value to `envoy-dispatch` as
`ENVOY_TOKEN`. The listener runs as machine `<hostname>`, so the hub's user for this deployment
sets `dispatchMachineId` to the component's `hostname`. Both Envoy containers mount the NATS seed
at `/var/run/secrets/envoy/nats-seed`; the seed remains outside Pulumi state. With
`agentSecretsUrl` set, the `envoy-dispatch` container alone also mounts its projected
ServiceAccount token, read-only, at `/var/run/secrets/dispatch/agent-secrets-account-token`; with
`azureEmbedKeySecret`, the Azure key at `/var/run/secrets/azure-embed/key`; and with the keyless
Azure form, the projected token at `/var/run/secrets/azure-federation/token`.

Each Deployment takes its update strategy from its own replica count. At one pod it is recreated,
stopping the old pod before the new one starts: a roll would run the outgoing and the incoming
builds together, which is safe only once the outgoing build follows the rules for two builds
running side by side and keeps its assets in the shared store, and a one-pod release cannot know
that of the build it replaces. Above one pod it rolls, with `maxUnavailable: 0` and `maxSurge: 1`,
so a new pod is ready before any old one goes down: an operator who runs a workload above one pod
runs such builds and wants no gap in service. There is no strategy or surge input, since the safe
strategy follows from the count. So at one replica an input or image update recreates that
workload's one pod, which is a brief outage, and the front door, at its default of two, rolls.

Every pod carries two rules over its own Deployment's pods, at any replica count. Required pod
anti-affinity on `kubernetes.io/hostname` puts at most one of them on a node, so a node's loss
takes one pod. A spread over `topology.kubernetes.io/zone` with `maxSkew: 1`, `whenUnsatisfiable:
DoNotSchedule` and `minDomains: 2` leaves a pod Pending rather than stacking a zone; `minDomains`
is what makes the spread hold on Autopilot, which creates nodes on demand while the scheduler
counts only zones that already have nodes, so without it two pods could satisfy the spread in one
zone. Old and new revisions count together, so a surge pod lands in another zone and on another
node than the pods it replaces. Autopilot raises a pod with pod anti-affinity to at least 500m CPU
and 512Mi memory, which is why the component refuses an app pod below that (above) and the
listener requests 500m and 512Mi.

A pod template whose Deployment has no PodDisruptionBudget carries
`cluster-autoscaler.kubernetes.io/safe-to-evict: "false"`: without it the cluster autoscaler
evicts the pod whenever it consolidates the pod's node. Autopilot honours the annotation as
[extended-duration](https://cloud.google.com/kubernetes-engine/docs/how-to/extended-duration-pods)
protection: for up to seven days, from scale-down and node auto-upgrade only (node repair, Compute
Engine maintenance and drains still evict), with each such pod on a node of its own whenever
possible and raised to at least 500m CPU and 512Mi memory. A template whose Deployment has a budget
(`appPdbMinAvailable`, `frontDoorPdbMinAvailable`, `listenerPdbMinAvailable`) carries no
annotation: the budget bounds every voluntary eviction, the autoscaler's among them, and lets the
autoscaler move those pods as far as the budget allows.

Every serving container has a startup and a liveness probe, and a liveness probe fails only when
the process does not answer at all. `envoy-dispatch`'s and `envoy-listener`'s run
`curl -sS -o /dev/null --max-time 8` against their own `/livez` on loopback (ports 8766 and
9020), with a 9-second timeout. `/livez` reads nothing: `envoy-dispatch` answers it `204`, and
`envoy-listener`, which has no such route, answers it `404` from its router, outside the `/v1`
routes its bearer guards. Without `-f`, curl exits 0 whatever the status, so any answer passes:
unlike `/healthz`, which reads Postgres and NATS on every call, the probe neither fails during a
dependency outage nor waits on a slow hub, so neither restarts every pod. Liveness asks every 10
seconds, and three missed answers in a row restart the container.
Startup asks the same question for longer before liveness takes over: 30 minutes for
`envoy-dispatch` (180 tries 10 seconds apart), since it migrates the database before it listens
and nothing bounds a migration's run time, and 5 minutes for `envoy-listener` (60 tries 5 seconds
apart). The `cloud-sql-proxy` sidecar runs with `--health-check` and serves its health endpoints
on port 9801: its startup probe (`/startup`, once a second for up to a minute) gates
`envoy-dispatch`'s start, and its liveness probe (`/liveness`, every 10 seconds, three failures)
restarts a hung proxy rather than leaving the pod unready for ever. The kubelet probes from the
pod's own node, which the app's NetworkPolicy does not fence, and no other pod reaches port 9801.
The front door's `tailscale` container's liveness probe runs
`tailscale --socket=<socket> version --daemon`, which asks tailscaled's LocalAPI for its status,
answered in every backend state: a node logged out, waiting for login or unapproved is alive, and
a restart would not help it, so only a minute with no answer (every 10 seconds, six failures)
restarts the container. Its startup probe is containerboot's `/healthz`, which answers once the
node has its tailnet addresses, given 3 minutes for a first login. `cert-seed` runs to completion
before tailscaled starts, and `cert-publish` and the advertiser have no liveness probe: they serve
no traffic, their loops log a failure and try again, and `cert-publish` exits on the one state it
cannot recover from, a certificate cache it cannot read.

Readiness is the other question. `envoy-dispatch` is ready while `curl -fsS` of its `/healthz`
succeeds (every 10 seconds, three failures), which reads Postgres and NATS. `envoy-listener` is
ready while `curl -fsS` of its `/healthz` succeeds every 5 seconds, two failures in a row taking
it out; that endpoint answers within 5 seconds, so the probe allows 6. A front-door pod is ready
while it hosts the Service ([The front door](#the-front-door)). The app and the listener reach
NATS only through the front door's SOCKS5 proxy, so with no front-door pod ready both fail
readiness within their probes' windows (30 seconds for the app, 10 for the listener) and leave
their Services, and the in-cluster `allowedNamespaces` consumers lose the app for that time as
well. It heals itself, since `internal/bus` reconnects without limit (`MaxReconnect = -1`) and no
liveness probe reads NATS.

A stopping app or listener pod keeps serving for five seconds after it is told to stop, which is
the window its removal from routing has to propagate, and is what its pre-stop wait (`sleep 5`)
is for. The kubelet runs that wait inside the pod's one `terminationGracePeriodSeconds` and
signals the container only once it returns, so each pod asks for 35 seconds. For the app, that is
the five of the wait added to the thirty `envoy-dispatch`'s own shutdown is built for (`stopGrace`
in `packages/envoy/cmd/dispatch/shutdown.go`) rather than taken out of them; whoever tunes either
number moves both, since a wait the grace period does not cover is paid for out of the server's
drain, which spends its document service's reserve and kills a slow write mid-commit. For the
listener, it is the five of the wait added to its own stop of about 26 seconds, with a margin.
Whether five seconds is enough to lose no request across a real release is unmeasured; the
consuming stack's release measures it, with a poll every 250 milliseconds. The front door drains
its node instead ([The front door](#the-front-door)).

Above one replica the app runs that many `envoy-dispatch` processes against one Postgres and one
NATS, and a rolling update runs the previous build beside the new one: "Running more than one
process" in `packages/envoy/cmd/dispatch/README.md` holds the preconditions. The component meets
the first, since every pod reads the one `DISPATCH_SIGNING_KEY` in the Secret
`<hostname>-dispatch`. The second bounds what a migration may change, since the rolling update
runs the new image's migrations while the previous build is still serving: "Database migrations"
in the same README holds the rule. Each process closes a client connection after its next response
once the connection is 60 seconds old, so after a roll the front door's and the in-cluster
callers' keep-alive connections spread onto the pods that have just come up.

Every container reads its environment once, at start, and Dispatch reads the Azure key file and
the NATS seed once, at boot, as the listener reads the seed, so a pod boots on a secret's new value
only when it restarts. The component's own Secrets restart their readers by themselves: each pod
template carries a `checksum/<secret>` annotation holding the resourceVersion of every component
Secret its environment reads, `<hostname>-dispatch` and `<hostname>-listener` on the app,
`<hostname>-listener` on the listener and `<hostname>-tailnet-auth-key` on the front door. The
Kubernetes provider replaces a Secret whose data changes, so a new value (a new
`signInClientSecret` or `tailnetAuthKey`, a replaced generated secret) gives the Secret a new
resourceVersion and rolls the pods that read it once the new Secret exists, and an unchanged
Secret never rolls a pod. The listener's bearer, `ENVOY_API_TOKEN` in `<hostname>-listener`, is on
both templates, since the listener serves with it and `envoy-dispatch` sends it as `ENVOY_TOKEN`:
the two Deployments roll in the same update, and until both have, a call from the app to a
listener pod on the other token is refused. `DISPATCH_AGENT_TOKEN` is also the `agentToken`
output, so a consumer that hands it to its agents passes them the new value too.
`<hostname>-tailnet-tls` carries no checksum: a renewed certificate rolls no pod.

The projected tokens need no restart either: the kubelet rewrites them in place, and Dispatch
reads the broker token on every call and the keyless Azure form's token on every exchange with
Entra. The secrets the component does not own carry no checksum, so after rotating one, restart
the Deployments that read it, once the synced Kubernetes Secret or the mounted file holds the new
value:

- the NATS seed: the app's `envoy-dispatch` and the listener's `envoy-listener` each read it once,
  at start
  (`kubectl -n <namespace> rollout restart deployment/<hostname> deployment/<hostname>-listener`);
- the GitHub App key and the Azure key: the app alone reads them
  (`kubectl -n <namespace> rollout restart deployment/<hostname>`). Rotate the App key by adding a
  new key to the App and writing it to the Secret Manager secret SecretSync copies, restarting the
  app, and only then deleting the old key from the App: GitHub accepts every key the App holds,
  and a pod keeps signing with the key it booted with until it restarts.

The front door reads none of these. A region change replaces the component's networked
resources. Keep `deletionProtection` enabled outside deliberate teardown-and-rebuild work:
deleting the Cloud SQL instance deletes its backups. A teardown therefore takes an `up` with
`deletionProtection: false` before `destroy`.

To inspect pending migrations before an image update, run the image with the deployment's service
account and `DATABASE_URL` from its Secret:

```sh
kubectl -n dispatch run census --rm -it --restart=Never --image=<envoyImage> \
  --overrides='{"spec":{"serviceAccountName":"<hostname>"}}' \
  --command -- /usr/local/bin/envoy-dispatch census
```

### The front door

The front door's `frontDoorReplicas` pods each run a tailnet node of their own, registered under
the pod's name (`TS_HOSTNAME` from `metadata.name`), and all of them host one Tailscale Service,
`svc:<hostname>`, so `https://<hostname>.<tailnetDnsName>` and the SOCKS5 proxy answer while any
one pod serves. No node holds the name `<hostname>`: it belongs to the Service. Each pod keeps its
node's state (machine key, node key, certificate cache) on its own `emptyDir`, with
`TS_KUBE_SECRET` set empty so containerboot keeps no Kubernetes state Secret, which every pod would
share: a restarted `tailscale` container comes back as the same node, and a new pod is a new node.
Every container of the pod runs as uid 999, since tailscaled writes its state and certificate
cache readable by its own uid alone and grants LocalAPI write access to its own uid, and drops
every Linux capability, since tailscaled runs in userspace.

`tailnetAuthKey` is the key each new node logs in with, once (`TS_AUTH_ONCE`). Mint it reusable,
since every new pod logs in with it; preauthorized, since a node waiting for an admin's approval
never serves; ephemeral, so the control server removes the node of a pod that died without its
logout; and with `tags`, which the Service's auto-approval names. Note its expiry and re-mint it
before then, since a pod started after it cannot join: a new `tailnetAuthKey` replaces the Secret
`<hostname>-tailnet-auth-key` and rolls the front door through its checksum. The Service must
exist in the tailnet, with an auto-approval in the tailnet policy for `tags`, before an apply
creates the pods (`tailnetService`, above).

No pod advertises the Service by itself (`TS_EXPERIMENTAL_SERVICE_AUTO_ADVERTISEMENT` is `false`).
Each pod's advertiser, a script on the Tailscale image in a container of its own, advertises it
once the pod's certificate gate lets it (below), watches every 5 seconds for the control server's
approval, then orders the Service name's certificate (up to 6 failed orders, with a jittered
backoff; a pod whose orders are spent drains the Service and stays unready), and writes the ready
file once the node is approved and tailscaled holds the certificate. The `tailscale` container's
readiness probe passes only while that file exists, so a pod counts toward a rolling update and
the budget, and receives SOCKS5 traffic, only while it hosts the Service. A node still unapproved
after 120 seconds is drained, and advertised again 15 seconds later, its bound doubling up to 16
minutes: that recovers a node that advertised before the Service existed. The advertiser removes
the ready file and starts over when the node changes, logs out or stops advertising the Service.

Every pod serves the Service name's certificate, and Let's Encrypt issues at most 5 certificates
for one exact set of names in 7 days, from every account together
([Rate limits](https://letsencrypt.org/docs/rate-limits/)). tailscaled caches the certificate in
its state directory and renews it at two thirds of its life, but a new pod starts with an empty
`emptyDir`, so without more every roll would order one per pod. The Secret
`<hostname>-tailnet-tls` keeps one for the pods instead:

- `cert-seed`, an init container on the Tailscale image, copies the published certificate, its key
  and the ACME account key into tailscaled's cache before tailscaled starts;
- the claim gate, in `cert-publish`, lets the pod advertise only with a certificate for the
  Service's name more than 31 days from its end (one no pod renews yet), cached or published, or
  with one of 3 claims per 7 days recorded in the Secret's `legion.dev/certificate-claims`
  annotation, each write conditional on the resourceVersion it read so two pods never take one
  claim. Three leaves room under Let's Encrypt's five for an operator's own order or a retried
  apply. A pod that finds no claim left does not advertise, so orders nothing, and stays unready,
  asking again every 30 seconds. The gate also records each pod's node in the
  `legion.dev/front-door-nodes` annotation. Its read every 30 seconds asks for the Secret's
  metadata alone (`PartialObjectMetadata`), never its certificate key;
- `cert-publish`'s loop, every 60 seconds, writes the pod's own cached certificate into the Secret
  when it ends later than the published one for the same name, so the next pod finds it.

Every front-door pod runs tailscaled's SOCKS5 proxy on `0.0.0.0:1055`, and the Service
`<hostname>-tailnet` sends the app's and the listener's NATS connections to whichever pods are
ready. Readiness, which hosting the Service decides, gates the proxy too, so a pod that cannot
host the Service (its claims spent, the Tailscale Service missing or unapproved) also carries no
NATS traffic. That coupling is accepted: in steady state at least one pod is ready, and the SOCKS5
path is to give way to an in-cluster NATS Service.

The front door's grace period is 60 seconds. Its `tailscale` container's preStop hook takes the
node out of the Service and off the tailnet before the pod goes, so a replaced pod leaves no
offline node approved behind: it marks the node as draining and removes the ready file, runs
`tailscale serve drain svc:<hostname>` (at most 5 seconds), waits 20 seconds for clients to stop
choosing this host (containerboot's own figure: about 15 for the control server to choose a new
primary and 5 to spare), and runs `tailscale logout` (at most 5 seconds). Those 30 seconds and
containerboot's own shutdown, at most 25, leave 5 seconds of the 60 for tailscaled to exit before
the kubelet's SIGKILL.

### First deployment

The first Google Cloud deployment proves the Autopilot admission of both native sidecars, the
Secret Manager CSI mount, the Auth Proxy's private-IP connection, the cluster's IP allocation,
tailnet Serve for HTTPS, SSE and websockets, the SOCKS5 connection to the hub, the non-root image
user, and the exec-plugin credentials on the machine that runs Pulumi. It also verifies that the
pod is ready, Dispatch reports a schema version, the site signs in through the configured client,
and the database retains backups and point-in-time recovery.

### Moving from a single front-door node

A deployment applied before the front door hosted a Tailscale Service runs one front-door pod
whose node holds the name `<hostname>` and keeps its state in the runtime Secret
`<hostname>-tailscale-state`. The move is an outage, from the moment the old node is gone until a
new pod hosts the Service. The update itself deletes the Role and RoleBinding
`<hostname>-tailscale-state` and changes Deployment `<hostname>-tailnet` in place, since its name
and selector are unchanged. Before it:

1. Delete the tailnet machine that holds the name `<hostname>`: Tailscale refuses a Service whose
   name a machine holds. Dispatch is off the tailnet from here on.
2. Declare `svc:<hostname>` in the tailnet, with an auto-approval in the tailnet policy for the
   front door's `tags`, and pass the Service's `Name` output as `tailnetService`.
3. Set `frontDoorToolsImage`, and give `tailnetAuthKey` a key minted as the front door needs it
   ([The front door](#the-front-door)).
4. Optionally seed `<hostname>-tailnet-tls`, so the first pods order no certificate; unseeded,
   they spend one or two of the three weekly claims. The component creates that Secret empty, and
   its provider does not adopt one made by hand, so create it first with an update that targets it
   alone (`pulumi up --target '<the Secret's URN>'`), then write into it, from
   `<hostname>-tailscale-state`, the value of `<hostname>.<tailnetDnsName>.crt` as `tls.crt`,
   `<hostname>.<tailnetDnsName>.key` as `tls.key` and `acme-account.key.pem` as
   `acme-account.key.pem` (the keys tailscaled writes them under in a Kubernetes state Secret).
   The component ignores the Secret's contents, so the full update keeps them.
5. Before the update that sets `appPdbMinAvailable`, delete any budget made by hand named
   `<hostname>` (`kubectl -n <namespace> delete pdb <hostname>`): the component's app budget takes
   that name, and its provider does not adopt an object that already exists.
6. Back up, then delete, the runtime Secret `<hostname>-tailscale-state`, which Pulumi never owned
   and no pod reads any more.
7. Before switching `databaseAvailabilityType` in the same update, set `databaseTier` to the tier
   the instance actually runs, so the instance update does not also resize it.

### YAML

```yaml
resources:
  gcp:
    type: pulumi:providers:gcp
  random:
    type: pulumi:providers:random
  dispatch:
    type: envoy-infra:gcp:Dispatch
    properties:
      region: <region>
      hostname: example-dispatch
      tailnetDnsName: example-tailnet.ts.net
      tags: [tag:example-dispatch]
      tailnetAuthKey: ${tailnetAuthKey}
      tailnetService: ${tailnetService.name}
      envoyImage: example/envoy@sha256:<digest>
      tailscaleImage: example/tailscale@sha256:<digest>
      cloudSqlProxyImage: example/cloud-sql-proxy@sha256:<digest>
      frontDoorToolsImage: example/envoy@sha256:<digest>
      natsUrl: ${hub.natsUrl}
      natsSeedSecret: projects/example-project/secrets/example-nats-seed
      signInClientId: <client-id>
      signInClientSecret: ${signInClientSecret}
      signInDomain: example.com
    options:
      providers:
        - ${gcp}
        - ${random}
```

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
| `ENVOY_SUBSCRIBE_ALL_TOPICS` | `1` when `subscribeAllTopics` is `true`; unset otherwise |
| `NATS_NKEY_SEED` | `seed`, when set. The whole environment is then a secret in the stack's state. |

The listener serves on the host's loopback, `127.0.0.1:<port>`. This version sets no listen host,
no API token and no webhook routes. The listener's other settings are on the documentation site's
[Envoy page](https://sjawhar.github.io/legion/dispatch/envoy/#running-the-listener).

The listener's outputs are `containerName` and `port`.

### Inputs

| Input | Default | What it is |
| --- | --- | --- |
| `machineId` | required | The listener's name, `ENVOY_MACHINE_ID`. It names the durable consumer `listener-<machineId>` and queue group `envoy-listener-<machineId>`; an interior ordinary space becomes a hyphen in both names. |
| `natsUrls` | required | The NATS servers to connect to. May come from another resource or stack. |
| `image` | required | The listener image, such as `ghcr.io/sjawhar/legion/envoy:<tag>`. |
| `containerName` | `envoy-listener` | The container's name. |
| `port` | `9020` | The port the listener serves and the health check calls. |
| `hostBridge` | unset | The address the listener delivers to this machine's sessions at: those registered through it, and those recording `machineId` as their host. Unset, the listener uses the host each session recorded, then `host.docker.internal`. Sessions that serve on the host's loopback are at `127.0.0.1`, since the container shares the host's network. |
| `subscribeAllTopics` | `false` | `true` sets `ENVOY_SUBSCRIBE_ALL_TOPICS=1`: the listener also subscribes to every notification topic, so a session served on this machine receives a topic the stream does not store. Only for a NATS user holding `HostPermissions`, such as a `gcp:NatsHub` user with `machineId`; under any other grant the server refuses the subscription and the listener's start ends. See [Permission sets](#permission-sets). |
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
- an empty `machineId`, one with leading or trailing whitespace, or one holding `.`, `*`, `>`, `/`,
  `\`, a control character, or whitespace other than an interior ordinary space, or one over 246 bytes: none can be used
  the durable consumer `listener-<machineId>` and queue group `envoy-listener-<machineId>` the
  listener uses;
- a `hostBridge` holding a control character, such as a line break;
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

A component creates its resources only with providers you pass it in its `providers` resource
option. The listener takes a `docker` provider always, and a `command` provider too when
`watchdog` is set; the hub takes a `gcp` provider; `gcp:Dispatch` takes `gcp` and `random`
providers. Their resources use those providers at the version and with the configuration you give
them: a docker provider's `host` decides which Docker daemon the listener runs on, and a gcp
provider's `project` which project the hub or Dispatch is created in. `gcp:Dispatch` creates its Kubernetes provider from the cluster it creates, so no default
Kubernetes provider is used. The plugin never falls back to a default provider of its own. Without
one, the component fails before it registers anything:

```text
envoy-infra:host:Listener needs a "docker" provider in its providers option
envoy-infra:gcp:NatsHub needs a "gcp" provider in its providers option
envoy-infra:gcp:Dispatch needs a "random" provider in its providers option
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

The examples use placeholders: `example-host-a`, `example-project`, the `*.internal.example` hosts
and each `<...>` stand for your own values. The hub's example is in
[docs/nats-hub.md](docs/nats-hub.md#a-nats-hub-in-yaml).

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
| `ENVOY_SUBSCRIBE_ALL_TOPICS` | `1` when `subscribeAllTopics` is `true` |
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
| `machineId` | required | `ENVOY_MACHINE_ID`, the listener's name. It names the durable consumer `listener-<machineId>` and queue group `envoy-listener-<machineId>`; an interior ordinary space becomes a hyphen in both names. |
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
| `subscribeAllTopics` | `false` | `true` sets `ENVOY_SUBSCRIBE_ALL_TOPICS=1`, as for `host:Listener`: only for a NATS user holding `HostPermissions`, since under any other grant the listener's start ends. |
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
  `webhooks` other than `[]` or exactly `["github", "slack"]`; an empty `machineId`, one with
  leading or trailing whitespace, or one holding `.`, `*`, `>`, `/`, `\`, a control character, or
  whitespace other than an interior ordinary space, or one over 246 bytes; a `hostBridge` or `listenHost` holding a
  control character; a `hostBridge` with leading or trailing whitespace; a blank `listenHost`; a
  `natsReplicas` below 1; an empty `subnetIds`; a `baseTags` holding `service`.
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
envoy-infra render webhook-listener-env --machine-id <id> --nats-url <url>... [--webhook <name>]... [--reviewer-app-id <id>] [--mention-trigger <t>] --listen-host <h> --host-bridge <h> --nats-replicas <n> [--subscribe-all-topics]
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

With `--subscribe-all-topics`, the component's `subscribeAllTopics: true`, it prints
`ENVOY_SUBSCRIBE_ALL_TOPICS=1` right after the `ENVOY_HOST_BRIDGE` line; without it, no such line.

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
line or part of an error. It has two stores, AWS Secrets Manager and Google Cloud Secret Manager:

```text
envoy-infra seed {check|generate|verify} --store aws-secrets-manager --name <secret id> [--region <r>] [--expect-account <id>] [--public-key U…]
envoy-infra seed {check|generate|verify} --store gcp-secret-manager --name projects/<project>/secrets/<secret> [--public-key U…]
```

With `aws-secrets-manager`, the command runs under your own AWS configuration, as the AWS SDK loads
it: the environment, then the shared configuration and credentials files. `--region` overrides the
region it names. Before any request or credential retrieval, it refuses a seed anywhere in that
configuration or in the endpoints it resolves from it, without echoing it (see "What `check` and
`verify` print"); then a Secrets Manager endpoint you configure (`AWS_ENDPOINT_URL`,
`AWS_ENDPOINT_URL_SECRETS_MANAGER` or a profile's `services` section) that is not `https`, unless
it is this machine's loopback address, since a seed would cross the network in plain text; and it
retrieves your credentials, and with none it can use, it stops with
`refused: <name>: no usable AWS credentials …`. With `--expect-account`, it then asks STS whose
credentials they are and refuses, before any other call, unless they are that account's; an empty
`--expect-account`, as `--expect-account "$ACCOUNT"` becomes with the variable unset, is a usage
error, never a skipped check. STS and Secrets Manager each resolve their own endpoint, so the
secret is checked too: `DescribeSecret`, every mode's first call, answers the secret's ARN, and the
command refuses a secret in another region than the one it sends to, and with `--expect-account`
one in another account, before it reads or writes a seed.
Pass `--expect-account` whenever you name a secret, so that a seed is never written to, or read
from, a secret of the same name in another account.

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
   secret that is absent, is scheduled for deletion or holds any version: the version
   `DescribeSecret` names as `AWSCURRENT`, before it lists any, so even when the versions listing,
   which Secrets Manager answers eventually consistently, does not show it yet, and otherwise any
   the listing shows, deprecated ones included, so it never replaces a seed a client may already
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
4. Put the public key into the NATS server's authorization as that user's `nkey`: on a
   `gcp:NatsHub` user (see [The hub's users](docs/nats-hub.md#the-hubs-users)), or with `hasSeed`
   set in an authorization (see [NATS authorization](#nats-authorization)). It is public, so it may
   sit in the stack's configuration in plain text.
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
`refused: <name> already serves version <n> by default (latest) …`, or, for a version that is not
enabled, `refused: <name> already holds version <n>, which latest names though it is <STATE> …`;
otherwise when the listing shows any; and, when it shows none, when an access of `versions/1`, the
number every secret's first version gets, finds a version in any state. Its write reads `versions/1`
by number once more just before `AddSecretVersion`, and refuses, writing nothing, when another
`generate` wrote this secret's first version after those checks:
`refused: add-secret-version <name>: <name>/versions/1 already exists; another generate wrote this
secret's first version, with another seed; nothing was written, and the public key above is not
stored`, exit 2, as Secrets Manager's `ResourceExistsException` is. It then reads version 1 back by
number, which must be the value it wrote, then `versions/latest`, the version Secret Manager serves
by default, which must be the one it added; while the listing shows no version yet, or `latest`
names none, it waits as it does on Secrets Manager. A `latest` that names a version it will not
serve, such as a disabled one, is no wait but an error after writing.

Secret Manager has no conditional add: `AddSecretVersion` takes no request id and no precondition.
So one window stays open, between a run's read of `versions/1` and its add, a single round trip:
when two `generate` runs for one secret both read in it before either adds, both adds land, the
first as version 1 and the other as version 2, which `latest` names and serves. `latest` is the
most recently created version whatever its state, so disabling or destroying version 2 alone leaves
no seed served. What each run reports:

- The run whose add landed as version 2 always stops with an error after writing, exit 1, that
  says what happened (another `generate` wrote version 1 first, so this run's version 2 is the one
  `latest` serves), what it means (the key the other run printed does not derive the served seed),
  and the commands that recover: keep this run's seed, by putting the key it printed in the NATS
  authorization and destroying version 1, or keep the other run's, by copying version 1 to a new
  version, as below, and destroying version 2; then `check`, and `verify` with the key kept.
- The run whose add landed as version 1 stops with an error after writing, exit 1, if its read-back
  sees version 2 (`<name> now holds 2 version(s), not only version 1: another writer raced this
  one`, or `latest` serving version 2); if version 2 lands after its read-back, it succeeds, and
  only the other run's message tells you its key is not the one served.

The copy, which pipes the seed between the two commands and prints nothing, is gcloud's own raw
read of a version:

```sh
gcloud secrets versions access <name>/versions/1 --format='get(payload.data)' \
  | tr '_-' '/+' | base64 -d \
  | gcloud secrets versions add <name> --data-file=-
```

The store takes that window over closing it with a claim on the secret, such as an
etag-guarded `UpdateSecret` before the add: the claim would need `secretmanager.secrets.update`,
beyond the roles below, and a run that died holding it would leave it for someone to clear before
any later `generate` could write.

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

The Secret Manager store refuses a seed, before any call, in each variable whose value it, or a
library it calls, sends in a request, dials as a host or quotes in an error:
`GOOGLE_OAUTH_ACCESS_TOKEN` (the bearer token), `GOOGLE_APPLICATION_CREDENTIALS` (quoted when the
file cannot be opened), `GOOGLE_CLOUD_QUOTA_PROJECT` (sent as `x-goog-user-project`),
`GOOGLE_CLOUD_UNIVERSE_DOMAIN` and `GCE_METADATA_HOST` (hosts it dials), and
`SECRET_MANAGER_ENDPOINT`. As for the AWS store, a seed is found anywhere in the value: whole,
inside a path, a token, a host or a project, or percent-encoded, as in an endpoint's path or query.
The refusal names the variable, as `refused: <name>: GOOGLE_APPLICATION_CREDENTIALS: an nkey user
seed, which is never Google Cloud configuration (not echoed)`.

### Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Done: `check` printed its line, `generate` wrote the seed, read it back and printed the public key and version id, or `verify` printed that the stored seed derives the key. A result that cannot be printed on stdout is never exit 0. |
| 1 | `verify` found another key, no seed, or a newest Secret Manager version that is not enabled; or `generate` failed after it began its write, including a version id it could not print, but not a write the store refused, which is exit 2. A version may then exist: `generate` has printed the public key, then the version id, `version id: unknown` when the write never answered, then `error after writing: <what failed>`. An interrupt (Ctrl-C) or `SIGTERM` during the write ends it the same way. Run `check`, and `verify` with that key, before anything else. |
| 2 | Refused before any write, printed as `refused: <reason>`, or a usage error. Nothing was written, so fix what the message names and run it again. A store call that fails before the write, such as one the credentials are not allowed, is a refusal naming the call, the secret and the error's code, AWS's or Secret Manager's gRPC status code, as `refused: describe-secret <name> failed: AccessDeniedException` or `refused: get-secret <name> failed: PermissionDenied`. So is a result `check` or `verify` cannot print, and a public key `generate` cannot print, which stops it before the write: `refused: <name>: could not print the public key: <error>; nothing was written`. So is the first write the store refuses because another `generate` already wrote this secret's first version, Secrets Manager's `ResourceExistsException` or Secret Manager's `versions/1` found just before the add: it comes after the public key is printed, but no version holds that key, and no `version id` line follows: `refused: put-secret-value <name> failed: ResourceExistsException; another generate wrote this secret's first version, with another seed; nothing was written, and the public key above is not stored`, or `refused: add-secret-version <name>: <name>/versions/1 already exists; …` on Secret Manager. Run `check`, then `verify` with the other run's key. |

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
| `generate` | `GetSecret`, `GetSecretVersion`, `ListSecretVersions`, `AccessSecretVersion` on `versions/1`, and again just before `AddSecretVersion`, `AccessSecretVersion` on `versions/1`, then `ListSecretVersions` and `AccessSecretVersion`, with `AccessSecretVersion` on `versions/1` between them when the listing shows no version, again after each pause while Secret Manager has not caught up with the write, and `GetSecretVersion` when the last answers `FAILED_PRECONDITION` | `secretmanager.secrets.get`, `secretmanager.versions.get`, `secretmanager.versions.list`, `secretmanager.versions.add`, `secretmanager.versions.access` |

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
server's configuration is rendered from it, so it holds public keys only. `aws:NatsService` takes
it as its `authorization` input, an `aws:Authorization` of `aws:User`s, with the properties below;
`envoy-infra render nats-start-script` reads the same from its file, keyed the same way. A user's
`publish` and `subscribe` are each a `nats:Rules`, the one rules type every NATS component takes,
whatever its cloud: `nats.RulesArgs` in the Python and Go SDKs. `gcp:NatsHub` renders its server's
from its `users`, `listener` and `sessionUser` (see [The hub's users](docs/nats-hub.md#the-hubs-users)); `host:Listener` runs no
NATS server and takes none.

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

### Permission sets

`core` also builds the grants Envoy's own clients need from Envoy's own names
(`packages/envoy/internal/bus` and `packages/envoy/internal/contracts`), so no consumer spells
Envoy's subjects. Each is a `core.Permissions`, a publish and a subscribe `Rules` that a user takes
as its `Publish` and `Subscribe`. Below, `<m>` is the machine id with each space as `-`.

`ListenerPermissions(machineID)` is what `envoy-listener` needs as that machine. It publishes to:

- every notification topic, `notifications.>`: the subjects of the `ENVOY_NOTIFICATIONS` stream,
  the role lanes, every exception lane, and the ad-hoc topics `/v1/messages/publish` sends over
  core NATS (any `notifications.` topic the stream does not capture);
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
subscribes to `notifications.>` and `agentstream.*.control`. It does not subscribe to `_INBOX.>`,
so a client holding this set alone may answer a request but not make one: sessions that share one
user holding it, as a `gcp:NatsHub`'s `sessionUser` does, would otherwise each receive every other
session's replies, so request/reply from such a session is refused by design. A host's user,
holding `HostPermissions`, subscribes to `_INBOX.>` for its listener.

`HostPermissions(machineID)` is the grant of a host's one NATS user, which its listener and every
session on the host share: in each direction the listener's entries, then each of the session's
the listener's lack. A `gcp:NatsHub` user with `machineId` holds it, and a hub's own `listener`
holds `ListenerPermissions` for the hub's hostname. Sharing one user makes the host one trust
domain: any process that reaches the server as it, a listener's own process or any session on the
host, holds the whole grant above, host sessions included, since sessions run as the Unix user
that already reads the host's seed. This repository's listener and a session, connected as one
user holding `HostPermissions`, and connected as a user holding `ListenerPermissions` beside a
credential-free session user holding `SessionPermissions`, are checked against real nats-server
2.10 and 2.15.1 servers; `AGENTS.md` in this directory says how.

`HostPermissions` is also the one set under which a listener may set `ENVOY_SUBSCRIBE_ALL_TOPICS=1`
(`host:Listener`'s and `aws:WebhookListener`'s `subscribeAllTopics`). The listener then holds a
core subscription on `notifications.>` in its machine's queue group `envoy-listener-live-<m>`, so a
session served on its machine receives a topic the stream does not store, which the set's plain
`notifications.>` subscribe entry allows in any queue group. `ListenerPermissions` and
`DispatchPermissions` subscribe only to the role lanes and `_INBOX.>`, so the server refuses that
subscription, and the listener's start ends with exit 1 and an ERROR naming
`ENVOY_SUBSCRIBE_ALL_TOPICS`. A `gcp:NatsHub`'s own `listener` and `gcp:Dispatch`'s listener
therefore never set it, and neither component takes the input. Both outcomes are checked against
the real servers above: the listener opted in as a user holding `HostPermissions` delivers an
ad-hoc topic to a session's serving port with no refusal, and opted in as a user holding
`ListenerPermissions` ends its start, the server's one refusal its subscription to
`notifications.>`.

`DaemonPermissions()` is the grant of a Legion daemon's dedicated NATS user. It publishes the
notification stream's info request, the info, create and next-message requests of any consumer of
that stream, and their acknowledgements. It subscribes to the daemon's role-lane delivery
exceptions and to reply inboxes. The consumer subjects are not narrowed to the daemon's own two,
so its seed can read, recreate and drain every consumer of the stream, each host's listener
consumer included, and create a push consumer that delivers the stream's messages to any subject,
one it may not publish to included. Its `_INBOX.>` subscription receives every reply any client
on the server gets, key-value reads and watch updates included, such as the listener's
`envoy_roles` and `envoy_sessions` watchers. A `gcp:NatsHub` user with `daemon: true` receives
this set; its seed belongs to the daemon, not to a host or session.

`DispatchPermissions(machineID)` is the grant of Dispatch's one NATS user, shared by
`envoy-dispatch` and the listener beside it. It adds to `ListenerPermissions(machineID)` the info,
filtered create, delete and acknowledgements of Dispatch's `delivery-events` and `delivery-jobs`
durables; the key-value calls for `envoy_webhook_redelivery`; publication of every session's
agent-stream control subject; and subscription to every session's agent-stream frames. A
`gcp:NatsHub` user with `dispatchMachineId` holds it. No session shares the user, so it holds none
of `SessionPermissions`: it is a direct publish to a session's agent-stream frames that is refused.
Whatever holds its seed, `envoy-dispatch` or the listener beside it, can still read every session's
frames and send control to any session; its filtered create on `delivery-events` lets it create
that durable on any filter, so it can read any retained notification; and, since NATS checks no
push consumer's deliver subject against a grant, a consumer it creates can deliver any payload onto
a session's frames. Its `_INBOX.>` subscription, `ListenerPermissions`'s, receives every reply any
client on the server gets, other listeners' durable push deliveries included. The real
`envoy-dispatch` runs under that grant against nats-server 2.10 and 2.15.1 in
`core/dispatch_permissions_live_test.go`, which also checks Dispatch's own user is refused a
publish to a session's frames.

A plugin release builds the sets from Envoy's names at its own commit, so a listener built from a
later commit that publishes or subscribes to a subject those names lacked needs a hub rendered by
a later release.

The listener's set covers a stream it creates, or one a listener of the same names created. A
stream an older listener created with the role lanes on it is migrated by the listener's start-up
(`packages/envoy/internal/bus/stream.go`, `migrateRoleLanesOffStream`), which lists the stream's
consumers, reads and updates every one, and purges the role lanes. The set grants none of those
calls, so on such a stream, as on a hub given a backup of one, the server refuses the migration.
Dispatch's start-up attempts the same migration (`bus.ConnectOwningStream`, which `envoy-dispatch`
calls too), and its grant adds none of those calls, so the server refuses it the same way.

## Changing the module

`AGENTS.md` in this directory covers the module's own `go.work`, the rules every component follows,
and how to run the unit and live tests.
