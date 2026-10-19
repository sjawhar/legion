---
title: Running Legion
description: What an operator needs to run the Legion daemon with its agents in Kubernetes, and how to configure, start, upgrade and watch it.
sidebar:
  order: 4
---

This page is for the operator: the person who runs the Legion daemon for a Dispatch project and
keeps it healthy. The daemon runs on a host (or in a pod) that the agents' pods can reach; every
agent it starts runs as an [Agent Sandbox](https://github.com/kubernetes-sigs/agent-sandbox) pod in
your cluster, from one published worker image.

## What you need

| Piece | What Legion needs from it |
| --- | --- |
| A Kubernetes cluster | Agent Sandbox installed, a gVisor runtime class, a node pool for Legion, a namespace and a storage class ([below](#the-cluster)). |
| A host for the daemon | Reachable from the pods on two ports: the API (`port`, 13370 by default) and the worker stream (`worker_stream_port`, the API port plus one). |
| Postgres | One database for the daemon's record (`postgres_dsn`, or `LEGION_POSTGRES_DSN` in its environment). |
| [Dispatch](/legion/dispatch/) | Its URL and a bearer token for Legion's agents, and the project's repositories mapped to it in Dispatch's Settings → Repositories → Projects, one of them the project's default ([below](#the-projects-repositories)). |
| The Envoy listener and NATS | The listener's URL and API token, and the NATS URLs. The daemon reads Dispatch's issue events and each repository's GitHub events from Envoy's notification stream on NATS, and reaches agents through the listener. [Envoy](/legion/dispatch/envoy/) covers how a repository's webhooks reach the listener and the NATS subjects the daemon's user needs. |
| Three GitHub Apps | An **implement** App (the implementer's and merger's identity), a **review** App (every other role's) and an **Envoy** App (the webhook that feeds Envoy, and Dispatch's GitHub reads), each installed on the owner of every repository Legion works, with its private key ([below](#the-github-apps)). |
| The worker image | `ghcr.io/sjawhar/legion-worker`, pinned by digest. |
| A model route | An Oh My Pi provider file and settings overlay, and the credential they read ([below](#the-model-route)). |

## The project's repositories

A project's repositories are Dispatch's mapping, set on Dispatch's Settings → Repositories →
Projects page, one of them the project's default; `legion.yaml` names none. Each issue targets the
repository Dispatch resolves for it: its own (the issue header's Repository picker, or
`dispatch issue-update --repo owner/name`), else its nearest ancestor's, else the project's default.
The daemon records an issue on that repository, and its default branch as GitHub names it, when it
first records the issue, and keeps both: a later change of the issue's repository in Dispatch is
not applied, and the daemon says so on the issue. An issue whose repository resolves to none, or to
one the daemon does not serve, is not admitted, and the daemon says why on the issue. A claim an
operator starts on an issue the workflow does not record works the project's default repository as
it is at each launch, whatever repository Dispatch resolves for that issue: change the default
while an operator's tree runs and its next launch, or its pane's next GitHub token after a restart,
is for the new default, whose workspace it does not have. While the project serves no default, its
launches are refused and an adopted pane's gh keeps its last tokens until they expire.

The daemon reads the mapping at boot and again whenever it changes in Dispatch. It serves each
mapped repository both the implement and the review App are installed on, and leaves out one either
App is missing from: the boot logs it, `legion state --json` lists it under `repositories.refused`
with the App it lacks, and an issue on it is not admitted until the App is installed, when moving
the issue to `todo` again admits it. A project with no repository mapped does not boot; one whose
every repository lacks an App boots serving none and admits nothing until an App is installed.

## The GitHub Apps

Legion uses three GitHub Apps, each installed on the owner of every repository Legion works: an
implement App, a review App and an Envoy App. The daemon mints each agent's token from its role's
App, with the App's whole installation permissions, so the implement and review Apps' permissions
are exactly what Legion's agents can do on GitHub:

| App | Write | Read |
| --- | --- | --- |
| **review** (planner, tester, reviewer, architect) | contents, pull requests, checks, actions, issues | metadata, packages |
| **implement** (implementer, merger) | contents, pull requests, issues, actions, statuses, workflows | checks, metadata, packages |

The implement App cannot create a check run, and a review never comes from the App that wrote the
change. `gh api /apps/<app slug> --jq .permissions` shows what an App holds. The daemon itself reads
GitHub with the implement App too: every two minutes it reads each open pull request's base branch
and the checks that branch requires (its rulesets and its branch protection), the set the merger's
`READY` checks, since only those decide whether CI is red at a head.

`scripts/github-apps/create.ts` registers all three from the manifests beside it:
`scripts/github-apps/implement.json` and `review.json` hold the permissions above and carry no
webhook, and `scripts/github-apps/envoy.json` is the App whose webhook carries GitHub's events to
Envoy. That App only reads, and Dispatch reads GitHub with it. The script serves a page on this
machine's loopback address with one Create button per App. You click each and confirm on GitHub,
and the script writes GitHub's whole answer for each App (its id, private key, client secret and
webhook secret) to `<out>/implement.json`, `review.json` and `envoy.json`, readable only by you:

```sh
bun scripts/github-apps/create.ts --out <new dir> --name-prefix <prefix>- \
  --webhook-url https://<listener host>/webhook/github [--org <org>] [--public] \
  scripts/github-apps/implement.json scripts/github-apps/review.json scripts/github-apps/envoy.json
```

`--org` registers them under an organization, which only its owners and App managers can do;
without it they belong to the account that clicks. The Apps are private unless you pass
`--public`, and GitHub lets a private App be installed only on the account that owns it. Register
them under the one owner whose repositories Legion works, and keep them private. Pass `--public`
only when Legion works repositories under more than one owner: then any account can install them,
and an install of the Envoy App sends that account's repository events to your listener and adds
an installation that Dispatch searches.

`--webhook-url` becomes the Envoy App's webhook. That App's webhook secret is the listener's
`ENVOY_GITHUB_WEBHOOK_SECRET`, its client id is Dispatch's `DISPATCH_APP_CLIENT_ID`, and its
private key, base64-encoded, is `DISPATCH_APP_PEM_B64`. The review App's id is the listener's
`ENVOY_REVIEWER_APP_ID`. Store the private keys where `github_apps.<role>` and Dispatch read them,
delete the files, then install every App from `https://github.com/apps/<slug>/installations/new`
on the same repositories.

## The cluster

- **Agent Sandbox.** The daemon refuses to boot unless the CRD `sandboxes.agents.x-k8s.io` serves
  the version it speaks and the Deployment `agent-sandbox-system/agent-sandbox-controller` has an
  available replica. The refusal names every missing piece.
- **gVisor.** Every pod runs with `runtimeClassName: gvisor`.
- **A node pool for Legion.** Every pod selects nodes labelled `legion.dev/pool=legion` and
  tolerates the taint `legion.dev/pool=legion:NoSchedule`. You may add node selectors, tolerations
  and a priority class under `runtime.kubernetes.scheduling`, but not the pool label itself.
- **One reservation per pod, no affinity set by Legion.** Every container of a Legion pod carries a
  cpu and memory limit with a request no greater than it, and bounds its ephemeral storage — the
  node's disk its root filesystem writes to: `$HOME`, Oh My Pi's state home, the Go and Bun caches a
  build fills — with a per-role limit over a smaller request. A role's reservation is
  `runtime.kubernetes.resources.<role>` (`cpu` and `memory` the limits, `cpu_request` and
  `memory_request` what the scheduler reserves of them, `ephemeral_storage` the disk limit,
  `ephemeral_storage_request` what the scheduler reserves of it, each optional, each request at most
  its limit), and the daemon's defaults fill the rest — 750m and 6Gi for the implementer and
  tester; 750m and 4Gi for the reviewer; 250m and 1Gi for the architect, planner and merger; 1 CPU
  and 4Gi for the controller (`controller: daemon` only), each both request and limit; 20Gi of disk
  for the implementer and tester and 10Gi for every other role, over a 1Gi request each. A cpu or
  memory request the file leaves out equals the role's limit, so a deployment that sets none runs
  only `Guaranteed` pods. A lower request — `implementer: {memory: 32Gi, memory_request: 8Gi}` for
  an agent whose memory comes in bursts — makes every pod holding that role `Burstable`: more pods
  fit a node and a cluster that bills by requests bills less, but under node memory pressure the
  kubelet evicts a `Burstable` pod using more than its request before any `Guaranteed` one, or the
  kernel OOM-kills it, which stalls the tree until the pod is relaunched and its roles resume. The
  `resource-limits` row of `legion state --json` and `legion start --check-config` both name the
  roles that make a pod `Burstable`. On GKE Autopilot's general-purpose, `Balanced` and `Scale-Out`
  pods, a request below the limit is honoured only in a cluster that supports bursting
  (1.30.2-gke.1394000 or later on cgroup v2, with the `efficiency-daemon` DaemonSet running), and
  otherwise each limit is set to the request; even then such a pod, alone on its node, bursts only
  to about its own summed requests (21Gi for the implementer example above, never its 32Gi limit).
  A `Performance`-class pod bursts into its node's allocatable, but GKE sizes that node from the
  summed requests of its pods and DaemonSets, rounded up to a machine type, and bills that VM, so
  every Autopilot class sets the burst ceiling from the requests. To give a role real headroom on
  Autopilot, raise the pod's summed requests (the role's own request, or an idle sibling's) rather
  than its limit alone. See
  [Issue sizing](https://github.com/sjawhar/legion/blob/main/docs/kubernetes.md#issue-sizing-one-reservation-per-pod)
  for the sources. The image probe pod, which every
  boot runs before anything is served, carries a fixed reservation of its own and no role's share:
  250m and 1Gi over 5Gi of disk and a 1Gi request, since `legion probe-image` starts one Oh My Pi
  at a time and runs no lane or browser, and a probe sized as a role could keep the daemon from
  booting on a full pool. A pool with no room even for that does not stop the daemon: its probe pod
  stays Pending Unschedulable, it logs `boot probe is waiting on capacity` with the scheduler's
  reason and tries again every 5 minutes, serving nothing until the probe passes, and the
  `pool-capacity` row of `legion state`'s capability report then records how many attempts waited
  and why, open until `capabilities.decided.pool-capacity` names a reason. A six-role issue pod
  sums to 3 CPU and 19 GiB at the defaults, its disk limits to 80Gi over 6Gi requested. A container
  past its disk limit has its own pod evicted, the offending issue's alone, rather than the node
  reaching `DiskPressure` where another tree's pod can go; the 1Gi request reserves almost no disk,
  so a node's root volume can be oversubscribed by the pods' limits — an operator who knows the
  root volume raises `ephemeral_storage_request` toward a role's expected use so the scheduler
  reserves disk and places no pod a full node could not hold. Legion sets no affinity on a pod (GKE
  Autopilot adds its own to an issue or controller pod): each issue pod owns its volume, so the
  scheduler places it wherever the pool has room and Karpenter
  adds nodes under the pool's limits. Concurrent issue pods are bounded by what the pool's
  `limits.cpu` and `limits.memory` leave for pods of that sum (two per 8-vCPU floor node, about 80
  at `limits.cpu: 256`, or `limits.memory` / 19 GiB when that is smaller) once the probe pod and,
  under `controller: daemon`, the controller's pod are counted; `admission_cap` bounds roots alone,
  a tree of N children runs N+1 pods, and an issue pod the pool cannot place stays `Pending` until
  the daemon reads it dead and, once its launch failures run out, fails the claim.
- **A namespace and a storage class.** Each issue's volume (`issue_volume`, 20Gi by default; the
  daemon-launched controller's pod owns one too) comes from `storage_class`, which is required.
- **Pod Security.** Pods run under the `restricted` profile: user 1000, no privilege escalation, all
  capabilities dropped, the `RuntimeDefault` seccomp profile, and no automounted API token.
- **A ServiceAccount for the agents' pods**, named in `runtime.kubernetes.pod.service_account`
  (the namespace's `default` otherwise). It needs no permissions of its own; the model route can
  use its projected token as a credential.
- **An identity for the daemon**, in the kubeconfig and context `legion.yaml` names, with exactly
  these permissions in the namespace:
  - `sandboxes`: create, get, list, watch, patch, delete; `sandboxes/status`: get;
  - `secrets`: create, delete, update, get (never list);
  - `pods`: get, list, watch; `pods/log`: get;
  - `events`: list;
  - and a cluster-scoped get on the CRD `sandboxes.agents.x-k8s.io` and a get on the Deployment
    `agent-sandbox-controller` in `agent-sandbox-system`.

  The daemon never creates or deletes a pod and has no volume permissions: Agent Sandbox does
  both.

## The worker image

Every agent runs from `ghcr.io/sjawhar/legion-worker`, which carries Oh My Pi, Legion's two plugins
(`@sjawhar/pi-envoy` and `@sjawhar/pi-legion`), the `legion` CLI and a general toolchain (git, jj, gh,
Node, uv, the AWS CLI, kubectl).
`legion.yaml` accepts the image only by digest (`ghcr.io/sjawhar/legion-worker@sha256:…`). A digest
is published in each Worker Image workflow run's summary and in the body of each `legion-v<version>`
GitHub release; for a `sha-` or release-version worker-image tag,
`docker buildx imagetools inspect ghcr.io/sjawhar/legion-worker:<tag>` prints its digest. Worker-image
tags are `sha-<the commit's first 12 hex digits>` for every build and `<legion version>` for a
release. The package also currently holds `sha256-<image digest hex>` OCI referrer indexes from
earlier proof runs; they hold attestations, not worker images, so inspecting one does not print a
worker-image digest. A tag does not say which build published it, and a pull request's build publishes
a `sha-` tag too. Every build except a pull request's carries a GitHub artifact attestation, and
`docs/kubernetes.md` in the repository ("Pin by digest, never by tag") gives the `gh attestation
verify` command that accepts an image only when `main` built it from the commit its tag names.

If your repositories need more than the image carries, build your own image `FROM` it by digest,
put the extra commands in `/usr/local/bin` or `/usr/bin`, and pin `runtime.kubernetes.image` to your
image's digest.

## legion.yaml

The daemon reads one file, `legion.yaml`. Relative paths in it resolve against the file's own
directory. The repository's `deploy/kubernetes/daemon/legion.yaml.example`, rendered in the
[configuration reference](/legion/legion/reference/config/), is a complete file for a Kubernetes
deployment: copy it and replace every value that names your deployment (documentation addresses,
made-up App ids, an all-zero image digest, the example's `advertise_host`). `bind: 0.0.0.0` is
already real: a daemon whose own pod restarts onto a new IP binds every interface. As written, the
file passes the [configuration check](#check-and-start-the-daemon) once the three token files and
the kubeconfig it names exist and `LEGION_POSTGRES_DSN` (or `postgres_dsn`) names a Postgres.

What each part is for:

- **`project`** is the Dispatch project key, and it must also be a key of `projects`, which names
  each Dispatch project the daemon runs (its repositories are [Dispatch's mapping](#the-projects-repositories)). A project may name a
  `merge_queue_role`, a role that receives a copy of every `READY`, and a notice withdrawing it
  when the head's own CI turns red before the merge, or the head starts conflicting with its base.
  Its optional `merge.by_hand` mapping holds regular-expression paths, each matched anywhere in a
  changed path, so anchor one (`^migrations/versions/`) to mean a directory. A pull request whose
  change touches one is `READY` with a `MANUAL_MERGE_REQUIRED:` note: the merger does not auto-arm
  it, the packet the daemon tells starts with the note, and a person merges it under the
  repository's rules. Its optional `slack_channels` lists Slack channel ids (`C…` or `G…`) whose
  mentions of the Legion Slack app reach the project's controller, which answers in the thread. It
  needs `controller: daemon` and `runtime.kubernetes.agent_secrets`, and the loader refuses it
  otherwise: the controller replies with the Slack bot token the secrets broker grants only to the
  Legion daemon service's own pods
  ([Envoy](/legion/dispatch/envoy/#how-slack-mentions-arrive-and-how-an-agent-replies) says how
  the mentions arrive and where the controller's reply token is stored). A channel belongs to one
  project: the loader refuses one named twice. A daemon restarted with a changed list relaunches the
  controller, which resumes its session hearing the new list.
- **`daemon_url`**, **`envoy_url`**, **`dispatch_url`**, every **`nats_urls`** entry and the
  worker-stream host (**`advertise_host`** when set, **`bind`** otherwise) are handed to pods, so
  none of them may be a loopback or unspecified address. `advertise_host` (optional, Kubernetes
  only) is an IP address or a DNS name, such as the Service that fronts a daemon whose own pod
  restarts onto a new IP. Beside it, `bind` is only where the daemon listens: it may be `0.0.0.0`,
  but not loopback, where no pod reaches it.
- **`github_apps`**: each App takes exactly one of `private_key` (the PEM itself),
  `private_key_command` (a command whose output is the PEM) or `private_key_secret`. The daemon
  finds each App's installations itself; `installations` (owner to installation id) is optional.
- **`gates.design`**: `root-issues` asks a person to approve every root issue's spec before any
  phase starts; `off` skips the approval. There is no other way past the gate.
- **`instructions`** (optional) names a Markdown file of your standing rules (required checks,
  deploy and smoke commands, scope) that is appended to every agent's system prompt. It must hold no
  secret.
- **`provider_keys`** (optional) maps a variable Oh My Pi reads to a key of the providers Secret
  ([below](#secrets-and-files)); each pod gets those keys alone, in Oh My Pi's environment.
- **`nats_nkey_seed_file`** and **`nats_daemon_nkey_seed_file`** (optional) are the NATS nkey user
  seeds for the agents and for the daemon's own connection, each in a file only its owner can read.
- **`runtime.kubernetes.agent_secrets`** (optional) enrolls every pod with the
  [Secrets Broker](/legion/broker/): `url` is the broker, and `token_file` is where the daemon's own
  projected ServiceAccount token for the broker's audience is mounted. The daemon logs in with that
  token at every start and keeps logging in; no person approves it, provided the broker's
  `BROKER_SERVICE_LOGINS` names the daemon's ServiceAccount.
- **`controller`** (optional) is who runs the project's controller: `operator` (the default), you,
  with `legion controller start`, or `daemon`, a pod the daemon launches and keeps running
  ([Start the controller](#start-the-controller)).
- Workflow limits, all optional: `linger_hours` (72), `review_round_cap` (3), `max_fix_attempts`
  (3), `controller_wake_interval_seconds` (3600), and the supervision budgets
  `launch_failure_limit` (3), `prompt_failure_limit` (3) and `prompt_retire_limit` (2).

The loader refuses any key it does not know, naming it, and refuses `omp_invocation` and
`omp_launch_prefix` under Kubernetes, since every pod runs the image's Oh My Pi.

## Secrets and files

- **The operator token** authenticates `legion controller start`, `legion claims` and
  `legion status <issue> <status>` from an operator's shell. Make one long random string in a file
  only you can read:

  ```sh
  openssl rand -hex 32 > operator-token && chmod 0600 operator-token
  ```

- **The Dispatch and Envoy tokens** go in the files `dispatch_token_file` and `envoy_token_file`
  name. The daemon hands each pod its own copy; no pod reads your files.
- **The providers Secret**, `legion-<project>-providers` in the namespace (`<project>` lowercased
  with everything but letters and digits removed, `legion-widgets-providers` here), holds the keys
  `provider_keys` names, and `NATS_NKEY_SEED` (the agents' seed) when you use one. You create it;
  the daemon never does, and a key the kubelet cannot mount refuses the boot.

## The model route

Legion names no model and no provider: how an agent reaches a model is yours. The repository's
`deploy/kubernetes/operator-route/` is a working example, rendered in the
[configuration reference](/legion/legion/reference/config/):

- `models.yml`, an Oh My Pi provider file whose API key is a command reading a mounted credential;
- `overlay.yml`, an Oh My Pi settings overlay that sets each role's model (`modelRoles`), holds
  every session to the route's models, and disables every provider the route does not use;
- `pod.yml`, the `runtime.kubernetes.pod` block that mounts both, and the credential, into every pod.

To stand it up:

1. Copy `models.yml` and `overlay.yml` into a directory of your own, and put your model endpoint's
   base URL in place of `${MODEL_BASE_URL}` in `models.yml`.
2. Write both into the ConfigMap `legion-operator-route` in the `legion` namespace:

   ```sh
   deploy/kubernetes/operator-route/apply.sh --context <kube context> <directory>
   ```

   `--context` is required, and the script names the cluster it wrote to. It refuses a
   `models.yml` that still holds the placeholder. It writes with a server-side apply that forces
   conflicts, so it takes the two keys over from a tool that wrote the ConfigMap before, and names
   that tool's field manager on stderr. A tool that still declares the ConfigMap acts at its own
   next apply: a forced one writes its copy back, and one without force fails on any key whose value
   differs and leaves yours. Either way, stop that tool declaring the ConfigMap first.
3. Put `pod.yml`'s contents under `runtime.kubernetes.pod` in `legion.yaml`, with your endpoint's
   token audience in place of `${MODEL_TOKEN_AUDIENCE}`.

Name every role Legion's agents use in `modelRoles` (`default`, `review`, `oracle`, `deep`, `smol`,
`slow`, and the rest the example names): the daemon refuses to boot when an agent's role has no
model, or its model's key does not work, rather than let it run silently on another model. Set
every variable your route depends on in `pod.env`, which outranks a repository's own `.env`.

To change a role's model later, edit its line under `modelRoles` in your `overlay.yml` and run the
same `apply.sh` command. Pods started after that use it; a running pod keeps its files until it is
relaunched.

## Check and start the daemon

Validate the file first. `--check-config` makes every refusal boot would make from the
configuration, the environment and the files it names, without starting anything, running a key
command or writing a file:

```sh
legion start --config legion.yaml --check-config
# Config OK: project=WIDGETS
```

Then start it:

```sh
legion start --config legion.yaml
```

The daemon runs in the foreground and logs JSON lines to standard error. At every boot it checks
that Agent Sandbox is installed and runs the worker image's launch probes in a probe Sandbox
(`legion-probe-<project>-<digest prefix>`): the image's Legion plugin must speak this daemon's contract
and load with its Envoy plugin,
every agent's model must resolve, and every model key must work. Nothing restarts the daemon on its
own, so run it under a process supervisor you trust. `legion stop --config legion.yaml` stops it,
and `legion legions` lists the daemons registered on the machine.

The `legion` binary the daemon runs and the worker image must come from the same commit. The
image carries that binary, which embeds Legion's role prompts, at `/opt/legion/bin/legion` and
Legion's two Oh My Pi plugins at `/opt/legion/pi-envoy` and `/opt/legion/pi-legion`, so you can
take all three from the image you pinned:

```sh
image=ghcr.io/sjawhar/legion-worker@sha256:<digest>
id=$(docker create --platform linux/amd64 "$image")
docker cp "$id:/opt/legion/bin/legion" ./legion
docker cp "$id:/opt/legion/pi-envoy" ./pi-envoy
docker cp "$id:/opt/legion/pi-legion" ./pi-legion
docker rm "$id"
```

The binary is static and built for linux/amd64. Elsewhere, build `legion` from the same commit
with Go.

## Start the controller

The controller runs in one of two places, as `controller` in `legion.yaml` says: on your own
machine, in your terminal, which is the default (`controller: operator`), or in the cluster, as a
pod the daemon launches and keeps running (`controller: daemon`).

### In the cluster

Set `controller: daemon` in `legion.yaml` (it needs `runtime: kubernetes`) and restart the daemon.
It launches the controller at boot as an Agent Sandbox pod, `legion-<project>-controller`, with the
same model access as the workers and a volume of its own for its session, relaunches it when it
dies, and resumes the same session. You start nothing, and `legion controller start` against this
daemon is refused: one controller runs per project. Nobody types into the pod: reach the controller
through Dispatch (a message to its session on the Agents page, a reply to an ask it opened, a
mention) and read its session with `kubectl logs <pod> -c controller`, its one container's log. A
Send from the Agents page is answered in the conversation that page shows.
`legion claims list` shows its claim.
Size its pod under `runtime.kubernetes.resources.controller`: with no requests it is the first pod
the kubelet evicts under memory pressure.
To hand the controller back to a person, set `controller: operator`, drop
`runtime.kubernetes.resources.controller` (the daemon refuses that key unless `controller: daemon`)
and restart the daemon: at boot it stops its own controller's claim and releases the pod, deleting
the pod's volume and the session on it, and you then start the controller on your machine as below.
Switching to `controller: daemon` again starts a fresh controller, not the old session.

A daemon built before LEGION-592 cannot run against a database a `controller: daemon` daemon used:
it lists the controller's claim as an issue keyed `""`, which every agent's plugin refuses. So to
stop using the in-cluster controller, switch to `controller: operator` on this release rather than
rolling back the image. If you must roll back, do it in this order:

1. Remove `controller` and `runtime.kubernetes.resources.controller` from `legion.yaml`: the
   earlier release refuses both as unknown keys, and without them this release runs
   `controller: operator`. Restart this release once with that file. Do not skip this boot: only
   this release ends the stopped controller's registration. It is done when `legion claims list`
   shows `legion-<project>-controller` `retired`. If the boot is refused,
   stop and follow the refusal's entry in [Troubleshooting](/legion/legion/troubleshooting/).
2. Stop that daemon: the earlier release must not run beside it for the same project, and while it
   runs it still holds the claim in memory, so any write of that claim would put the row back.
3. `<project>` is your `project` lowercased with every character outside `a-z0-9` dropped
   (`project: LEGION` gives `legion-legion-controller`). Run
   `select token, state from claims where token = 'legion-<project>-controller';`, which must
   return exactly one row, `retired`; then
   `delete from claims where token = 'legion-<project>-controller';`, which must answer
   `DELETE 1`. Never delete by role: other projects' daemons may share the database. If either
   answer is anything else, do not start the earlier release.
4. Start the earlier release.

`docs/kubernetes.md` in the repository ("Daemon-launched controller") explains why.

### On your machine

The controller runs in your terminal as an interactive Oh My Pi session. It reads a small file of
its own, never `legion.yaml`; the repository's `deploy/kubernetes/daemon/controller.yaml.example` is
the complete shape (rendered in the [configuration reference](/legion/legion/reference/config/)):

1. Install Oh My Pi, then Legion's two plugins from the same image as the daemon
   (`omp plugin install ./pi-envoy && omp plugin install ./pi-legion`, copied out as above). Set
   `omp_invocation`, or `LEGION_OMP_PATH` to the absolute path of `omp`.
2. Copy the example, set `project` to the daemon's project and `daemon_url` to the daemon's API as
   your machine reaches it (through a port-forward or a tunnel if need be), and put the operator
   token, the Envoy token, the agents' NATS seed (optional) and a Dispatch token for the controller
   in files only you can read.
3. Start it:

   ```sh
   legion controller start --config controller.yaml
   # or, with the daemon behind a port-forward:
   legion controller start --config controller.yaml --daemon-url http://127.0.0.1:13370
   ```

The command checks the file and the plugins, fetches a fresh controller credential from the daemon
with your operator token, then fetches the review App's token for the controller's `gh` and keeps
it refreshed while the session runs (against a daemon with no GitHub App, it says so and the
controller's `gh` acts as nobody), and starts Oh My Pi in the foreground; its first turn runs the
controller's start procedure with nothing typed. Running it again replaces the previous controller.
Closing the terminal leaves the project without one, and the daemon logs
`controller not registered; run legion controller start` until someone starts it.

## Upgrade

1. **Pick the new image** and note its digest ([The worker image](#the-worker-image)), and take the
   `legion` binary and both plugins from it.
2. **Update `legion.yaml`**: set `runtime.kubernetes.image` to the new digest and run
   `legion start --config legion.yaml --check-config`.

   **After the first upgrade to a release whose repositories are Dispatch's mapping,** a
   `legion.yaml` that still sets `projects.<KEY>.repo` is refused at load with
   `Unknown key "projects.<KEY>.repo": the project's repositories are Dispatch's mapping`. Upgrade
   Dispatch first, map the project's repositories there with the old `repo` as the project's
   default, then delete the key and upgrade the daemon: its first boot records each issue it
   already runs on the repository Dispatch resolves for it, and refuses to boot, naming every
   issue, while one resolves to none.

   **After the first upgrade to a release without `review_workflows`,** a `legion.yaml` that still
   sets `projects.<KEY>.review_workflows` is refused at load with
   `Unknown key "projects.<KEY>.review_workflows"`, by `legion start --check-config` and by the
   daemon's boot alike, so the daemon does not come up on it: delete the key before or with the
   image pin (an empty list is refused too). A review bot's red is now CI like any other: it sends
   the work back to the implementer, which answers and resolves the bot's threads and pushes, and
   the bot runs again; the reviewer adjudicates nothing of it and re-runs nothing. Where the bot's
   threads must be closed before a merge, make that the repository's own rule (a ruleset's
   "Require conversation resolution before merging").

3. **Restart the daemon** with the new binary: stop it through your supervisor (or
   `legion stop --config legion.yaml`) and start it again. `legion restart <PROJECT>` does both in
   the shell it runs in, from the configuration the stopped daemon recorded. The new daemon probes
   the new image before it serves.
4. **Running agents carry on.** The restarted daemon re-adopts every running pod and its session. A
   pod keeps the image it started with; every pod the daemon starts afterwards, for the next phase
   or a resumed worker, runs the new one. To move a long-running agent over now, suspend and resume
   its claim:

   ```sh
   legion claims list    --config legion.yaml --operator-token-file operator-token
   legion claims suspend --config legion.yaml --operator-token-file operator-token --claim <token>
   legion claims resume  --config legion.yaml --operator-token-file operator-token --claim <token>
   ```

   An operator's suspension of an agent in a turn waits for that turn to land, at most
   `worker_stop_timeout_seconds`, and the cut-off task is re-sent on the resume. One that reaches a
   claim while it is still launching waits until the agent is ready, or until its registration
   deadline. Until the stop lands the hold lives only in the daemon's memory, so before restarting
   the daemon wait until `legion claims list` shows the claim `suspended`, not merely
   `suspensionHeld`. The resumed agent continues the same conversation in a new pod.

   **An operator's suspension holds until the operator resumes the claim.** The daemon resumes a
   suspended claim on its own when its issue's phase moves to the claim's role (a phase start,
   re-admission, a child's return to `todo`), and keeps its own controller running, unless an
   operator suspended it; a phase start for a claim an operator suspended waits instead. At boot
   it also resumes a claim that owns its issue's current phase, but only one the daemon or the
   agent's own exit suspended. A notice for a suspended architect resumes nothing: it waits until
   the architect holds its role again, so an operator-suspended architect receives it when the
   operator resumes it. A drain therefore ends with the operator resuming every claim they
   suspended, one `legion claims resume` each; `legion claims list --json` names each one's
   `suspendedBy` (`operator`, `daemon`, `exit`, or `unknown` for one suspended before this
   release). The daemon's own suspension of a claim in a turn waits for the turn however long it
   takes, with no stop timeout: a child parked in Dispatch (`backlog` or `icebox`) stops its worker
   only once the worker's current turn ends, and a lingering tree's claims stay suspended until the
   tree is re-admitted.

   **After the first upgrade to a release that records `suspendedBy`,** every claim already
   suspended reads `unknown`. The next phase start, re-admission and the controller keeper resume
   those as before. The boot's own sweep does not, but a start already queued before the upgrade
   still resumes one at the first boot, as the earlier release would have. To keep a claim you
   drained before the upgrade held against every trigger, run `legion claims suspend` on it once
   the new daemon is up: that records the suspension as yours. Act only on the claims the state
   names as nothing will wake (`legion state`'s `claims nothing will wake` line, or
   `strandedClaims` in `--json`), and skip those you drained on purpose: resume a suspended
   architect, retry a failed one (`legion claims retry`, which relaunches its session with fresh
   budgets), and bring a suspended phase worker back as
   [A tree does not move after a restart](/legion/legion/troubleshooting/#a-tree-does-not-move-after-a-restart)
   shows for its `suspendedBy`. Never resume or retry by hand a claim of a lingering tree or of an
   issue parked out of the workflow: re-admission or the issue's return to `todo` starts it.

   **Rolling back across this release.** The earlier daemon boots on this schema but never writes
   `suspendedBy`, so a value this release wrote survives the earlier daemon's own suspends and
   resumes, an operator's included. Before starting this release again, run against the daemon's
   database, with no daemon of the project running, where `<project>` is your `project` lowercased
   with every character outside `a-z0-9` dropped (`project: LEGION` gives `legion`). Never run it
   without the project filter: other projects' daemons may share the database, and their own
   operator holds would turn `unknown`.

   ```sql
   update claims set suspended_by = 'unknown' where state = 'suspended' and project = '<project>';
   ```

   The boot sweep then resumes none of the claims the earlier daemon left suspended.
5. **Restart the controller** with the new binary and plugins.

## Observe

- **Is it running?** `legion status <PROJECT>` reads the machine's registry and the daemon's
  health endpoint:

  ```text
  legion WIDGETS: running — pid 41210, port 13370, healthy, started 2026-10-03T09:12:40Z
  ```

- **What is it doing?** `legion state --config legion.yaml` prints a summary:

  ```text
  legion WIDGETS on http://192.0.2.10:13370 — schema 12, boot 7 started 2026-10-03T09:12:40Z (first boot 2026-09-20T15:02:11Z)
  admission: 3 active, 1 waiting, cap 4; 9 issues
  pending Dispatch status writes: none
  held lifecycle rows: none
  claims nothing will wake: none
  ```

  `--json` prints everything the daemon records: `admission` (`cap`, `active`, `waiting`), each
  issue under `issues` with its `phase`, `status`, `repo` and `defaultBranch`, `holdReason`,
  `architect` and `workers` (each claim's `state`), `pullRequest` (`repo`, `head`, `checksVerdict`,
  `fixAttempts`) and `designGate` (`currentVersion`, `approvedVersion`), the `controllerLocator`,
  `pendingStatusWrites`,
  `heldOutboxRows` ([An issue's stop or start waits behind its own launch](/legion/legion/troubleshooting/#an-issues-stop-or-start-waits-behind-its-own-launch)),
  `strandedClaims` ([A tree does not move after a restart](/legion/legion/troubleshooting/#a-tree-does-not-move-after-a-restart)),
  `repositories` (the project's mapping as the daemon serves it: `owners`, `mapped`, and `refused`
  with each left-out repository's `reason`), and `agentSecretsLogin` when the broker is configured:

  ```sh
  legion state --config legion.yaml --json | jq '.issues["WIDGETS-12"] | {phase, status, holdReason, pullRequest, designGate}'
  ```

- **The daemon's log** (standard error) names every launch, death, refusal and relaunch; the lines
  [Troubleshooting](/legion/legion/troubleshooting/) quotes are the ones to search for.
- **Dispatch** shows each issue's status, the architect's questions and messages, `READY`, and the
  controller's daily report on the project's `Legion daily report` issue.
- **The controller's terminal** is where the controller says what it did each turn. You can type to
  it at any time; a message from you is always handled first.
- **The cluster** shows each issue as a Sandbox and a pod labelled with its tree and issue, with one
  container per role:

  ```sh
  kubectl -n legion get sandboxes,pods -l legion.dev/tree=WIDGETS-12
  kubectl -n legion logs <pod> -c implementer       # that role's launcher, its shim and Oh My Pi
  kubectl -n legion logs <pod> -c workspace-init    # the workspace provisioning
  ```

## Take a tree out

From an operator's shell, `legion status` sets a root's Dispatch status through the daemon, as the
controller does through its `legion` tool's `set_status`:

```sh
legion status WIDGETS-12 backlog --config legion.yaml --operator-token-file operator-token
```

`backlog` or `icebox` takes the tree out of the workflow: its agents are suspended and its slot goes
to the next issue. `todo` admits a labelled root again, as a new generation that starts from its
architect.
