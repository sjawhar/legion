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
| [Dispatch](/legion/dispatch/) | Its URL and a bearer token for Legion's agents. |
| The Envoy listener and NATS | The listener's URL and API token, and the NATS URLs. The daemon reads Dispatch's issue events and each repository's GitHub events from Envoy's notification stream on NATS, and reaches agents through the listener. |
| Two GitHub Apps | An **implement** App (the implementer's and merger's identity) and a **review** App (every other role's), each installed on the owner of every repository Legion works, with its private key. |
| The worker image | `ghcr.io/sjawhar/legion-worker`, pinned by digest. |
| A model route | An Oh My Pi provider file and settings overlay, and the credential they read ([below](#the-model-route)). |

## The cluster

- **Agent Sandbox.** The daemon refuses to boot unless the CRD `sandboxes.agents.x-k8s.io` serves
  the version it speaks and the Deployment `agent-sandbox-system/agent-sandbox-controller` has an
  available replica. The refusal names every missing piece.
- **gVisor.** Every pod runs with `runtimeClassName: gvisor`.
- **A node pool for Legion.** Every pod selects nodes labelled `legion.dev/pool=legion` and
  tolerates the taint `legion.dev/pool=legion:NoSchedule`. You may add node selectors, tolerations
  and a priority class under `runtime.kubernetes.scheduling`, but not the pool label itself.
- **One tree per node.** A tree's pods share one `ReadWriteOnce` volume, so they must run on the
  same node, and pods of different trees never share a node. Pods carry no resource requests unless
  you set them per role under `runtime.kubernetes.resources`, so the pool's node size decides what a
  tree gets: give every node room for a whole tree (four vCPUs is a good floor), and keep
  `admission_cap` at or below the number of nodes the pool can hold, or the extra trees' pods wait
  unscheduled.
- **A namespace and a storage class.** The tree volume (`tree_volume`, 20Gi by default) comes from
  `storage_class`, which is required.
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

Every agent runs from `ghcr.io/sjawhar/legion-worker`, which carries Oh My Pi, Legion's plugin, the
`legion` CLI, the role prompts and a general toolchain (git, jj, gh, Node, uv, the AWS CLI).
`legion.yaml` accepts the image only by digest (`ghcr.io/sjawhar/legion-worker@sha256:…`). A digest
is published in each Worker Image workflow run's summary and in the body of each `cli-v<version>`
GitHub release; for any tag, `docker buildx imagetools inspect ghcr.io/sjawhar/legion-worker:<tag>`
prints it. Tags are `sha-<the commit's first 12 hex digits>` for every build and `<cli version>` for
a release.

If your repositories need more than the image carries, build your own image `FROM` it by digest,
put the extra commands in `/usr/local/bin` or `/usr/bin`, and pin `runtime.kubernetes.image` to your
image's digest.

## legion.yaml

The daemon reads one file, `legion.yaml`. Relative paths in it resolve against the file's own
directory. This is a complete file for a Kubernetes deployment; every value in angle brackets is
yours:

```yaml
project: WIDGETS                       # the Dispatch project key this daemon runs
state_dir: ./state                     # the daemon's local state
# postgres_dsn: postgres://…           # or LEGION_POSTGRES_DSN in the daemon's environment
bind: <daemon host address>            # pods dial the worker stream here: never loopback
daemon_url: http://<daemon host address>:13370
admission_cap: 4                       # trees running at once

operator_token_file: ./operator-token  # the operator's bearer (below)
dispatch_url: https://<dispatch host>
dispatch_token_file: ./dispatch-token
envoy_url: http://<envoy listener host>:9020
envoy_token_file: ./envoy-token
nats_urls: [nats://<nats host>:4222]

projects:
  WIDGETS: { repo: <owner>/<repository> }
gates:
  design: root-issues                  # a person approves each root's spec (the default); or off
github_apps:
  implement:
    app_id: "<implement App id>"
    private_key_command: <command that prints the App's PEM private key>
  review:
    app_id: "<review App id>"
    private_key_command: <command that prints the App's PEM private key>

runtime:
  kubernetes:
    namespace: legion
    image: ghcr.io/sjawhar/legion-worker@sha256:<digest>
    storage_class: <storage class>
    tree_volume: 20Gi
    kubeconfig: ./kubeconfig           # the daemon's own identity
    context: <context>
    pod:                               # the model route: see below
      env:
        PI_CONFIG_FILES: /etc/legion-operator/overlay.yml
        CLAUDE_CODE_USE_FOUNDRY: "0"
      service_account: legion-worker
      volumes:
        - name: operator-route
          config_map:
            name: legion-operator-route
            items: [{key: models.yml, path: models.yml}, {key: overlay.yml, path: overlay.yml}]
        - name: operator-token
          projected:
            sources:
              - service_account_token: {audience: "<model gateway audience>", expiration_seconds: 3600, path: token}
      volume_mounts:
        - {volume: operator-route, mount_path: /home/legion/.omp/profiles/legion/agent/models.yml, sub_path: models.yml}
        - {volume: operator-route, mount_path: /etc/legion-operator/overlay.yml, sub_path: overlay.yml}
        - {volume: operator-token, mount_path: /var/run/operator}
```

What each part is for:

- **`project`** is the Dispatch project key, and it must also be a key of `projects`, which maps
  each Dispatch project the daemon runs to its GitHub repository. A project may name a
  `merge_queue_role`, a role that receives a copy of every `READY`.
- **`bind`**, **`daemon_url`**, **`envoy_url`**, **`dispatch_url`** and every **`nats_urls`** entry
  are handed to pods, so none of them may be a loopback or unspecified address.
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
  [Secrets Broker](/legion/broker/): `url` is the broker, `operator` the email of the person who
  approves the daemon's own machine login on Dispatch's credential page.
- Workflow limits, all optional: `linger_hours` (72), `review_round_cap` (3), `max_fix_attempts`
  (3), `controller_wake_interval_seconds` (3600), and the supervision budgets
  `launch_failure_limit` (3), `prompt_failure_limit` (3) and `prompt_retire_limit` (2).

The loader refuses any key it does not know, naming it, and refuses `omp_invocation` and
`omp_launch_prefix` under Kubernetes, since every pod runs the image's Oh My Pi. The
[configuration reference](/legion/legion/reference/config/) renders the example files the repository ships.

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
   `models.yml` that still holds the placeholder.
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
(`legion-probe-<project>-<digest prefix>`): the image's plugin must speak this daemon's contract,
every agent's model must resolve, and every model key must work. Nothing restarts the daemon on its
own, so run it under a process supervisor you trust. `legion stop --config legion.yaml` stops it,
and `legion legions` lists the daemons registered on the machine.

The `legion` binary the daemon runs and the worker image must come from the same commit. The
image carries that binary at `/opt/legion/go/bin/legion`, its role prompts at `/opt/legion/roles`
and Legion's Oh My Pi plugin at `/opt/legion/pi-legion-envoy`, so you can take all three from the
image you pinned:

```sh
image=ghcr.io/sjawhar/legion-worker@sha256:<digest>
id=$(docker create --platform linux/amd64 "$image")
docker cp "$id:/opt/legion/go/bin/legion" ./legion
docker cp "$id:/opt/legion/roles" ./role-prompts      # the daemon reads role-prompts/ beside its binary
docker cp "$id:/opt/legion/pi-legion-envoy" ./pi-legion-envoy
docker rm "$id"
```

The binary is static and built for linux/amd64. Elsewhere, build `legion` from the same commit
with Go, and point `LEGION_ROLE_PROMPTS_DIR` at the role prompts.

## Start the controller

The controller runs on your own machine, in your terminal, as an interactive Oh My Pi session. It
reads a small file of its own, never `legion.yaml`; the repository's
`deploy/kubernetes/daemon/controller.yaml.example` is the complete shape (rendered in the
[configuration reference](/legion/legion/reference/config/)):

1. Install Oh My Pi, then Legion's plugin from the same image as the daemon
   (`omp plugin install ./pi-legion-envoy`, copied out as above). Set `omp_invocation`, or
   `LEGION_OMP_PATH` to the absolute path of `omp`.
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

The command checks the file and the plugin, fetches a fresh controller credential from the daemon
with your operator token, and starts Oh My Pi in the foreground; its first turn runs the
controller's start procedure with nothing typed. Running it again replaces the previous controller.
Closing the terminal leaves the project without one, and the daemon logs
`controller not registered; run legion controller start` until someone starts it.

## Upgrade

1. **Pick the new image** and note its digest ([The worker image](#the-worker-image)), and take the
   `legion` binary, role prompts and plugin from it.
2. **Update `legion.yaml`**: set `runtime.kubernetes.image` to the new digest and run
   `legion start --config legion.yaml --check-config`.
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

   A suspension waits for the agent's current turn to land; the resumed agent continues the same
   conversation in a new pod.
5. **Restart the controller** with the new binary and plugin.

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
  ```

  `--json` prints everything the daemon records: `admission` (`cap`, `active`, `waiting`), each
  issue under `issues` with its `phase`, `status`, `holdReason`, `architect` and `workers` (each
  claim's `state`), `pullRequest` (`head`, `checksVerdict`, `fixAttempts`) and `designGate`
  (`currentVersion`, `approvedVersion`), the `controllerLocator`, `pendingStatusWrites`, and
  `agentSecretsLogin` when the broker is configured:

  ```sh
  legion state --config legion.yaml --json | jq '.issues["WIDGETS-12"] | {phase, status, holdReason, pullRequest, designGate}'
  ```

- **The daemon's log** (standard error) names every launch, death, refusal and relaunch; the lines
  [Troubleshooting](/legion/legion/troubleshooting/) quotes are the ones to search for.
- **Dispatch** shows each issue's status, the architect's questions and messages, `READY`, and the
  controller's daily report on the project's `Legion daily report` issue.
- **The controller's terminal** is where the controller says what it did each turn. You can type to
  it at any time; a message from you is always handled first.
- **The cluster** shows each agent as a Sandbox and a pod labelled with its tree, issue and role:

  ```sh
  kubectl -n legion get sandboxes,pods -l legion.dev/tree=WIDGETS-12
  kubectl -n legion logs <pod> -c worker            # the agent's shim and Oh My Pi
  kubectl -n legion logs <pod> -c workspace-init    # the workspace provisioning
  ```

## Take a tree out

From an operator's shell, `legion status` sets a root's Dispatch status through the daemon, as the
controller does:

```sh
legion status WIDGETS-12 backlog --config legion.yaml --operator-token-file operator-token
```

`backlog` or `icebox` takes the tree out of the workflow: its agents are suspended and its slot goes
to the next issue. `todo` admits a labelled root again, as a new generation that starts from its
architect.
