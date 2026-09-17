# The kind smoke: a throwaway Kubernetes instance of Legion

`up.sh` creates one self-contained test instance of Legion on this machine: a kind cluster of its
own, its own NATS and Postgres containers, its own Envoy listener and scratch Dispatch server on
its own ports, the in-cluster daemon from a **published worker image** (`SMOKE_WORKER_IMAGE`, a
digest — nothing is built or loaded), and one or more root issues released in a scratch Dispatch
project. `checkpoints.sh <name>` proves, one named assertion at a time, that the issue is admitted,
its agents run as pods, a crashed root pod comes back as the same agent one generation later, pods
carry their resource profiles and no secret in plain sight, and the running-worker cap holds.
`down.sh` tears down exactly what `up.sh` recorded. Everything the rig creates outside the cluster
is named by the instance and recorded under the instance's state directory; two instances on one
box never share a name, a port, or a record.

What comes from the checkout: the rig itself, the overlay template
(`deploy/kubernetes/daemon/overlays/kind`, copied and filled per instance), and the two Go binaries
`go build` produces from `packages/envoy` — the Envoy listener and the scratch Dispatch server, the
only two components of the instance not taken from a published image. What comes from the image:
the daemon and every pod it launches. A run therefore proves the checkout's listener and Dispatch
server beside main's released daemon image.

## Prerequisites

| tool | version used here | notes |
| :--- | :--- | :--- |
| docker | ≥ 24 | the kind node and the two containers |
| kind | v0.24.0 | `curl -Lo ~/.local/bin/kind https://kind.sigs.k8s.io/dl/v0.24.0/kind-linux-amd64 && chmod +x ~/.local/bin/kind` |
| kubectl | v1.31.0 | `curl -Lo ~/.local/bin/kubectl https://dl.k8s.io/release/v1.31.0/bin/linux/amd64/kubectl && chmod +x ~/.local/bin/kubectl`; its built-in kustomize renders the overlay |
| go | 1.26 | `packages/envoy` is `go 1.26.1`; on a box where go is a mise tool, run the rig under `mise x go@1.26 --` |
| bun | any current | the GitHub bridge (`SMOKE_GITHUB_INGRESS=envoy`), the controller CLI, the bridge test |
| tmux | any | the controller pane only (`-L legion-smoke-<instance>`) |
| jq, curl, openssl, ss, shred, setsid | coreutils/iproute2 | records, tokens, port checks, teardown |
| mise with the pinned Oh My Pi | `OMP_FORK_PIN` in `packages/daemon/src/daemon/omp-pin.ts` | required only when the checkout has `legion controller start` (pull request #1110) |

Plus: the sandbox repository (`SMOKE_REPO`, default `sjawhar/legion-smoke`) has both Legion GitHub
Apps installed; you hold the two App private keys and at least one model provider key; nothing of
the dev-box daemon is involved — the rig never starts, stops, or touches it.

## Inputs

Every input is an environment variable. Secrets reach `up.sh` **only through its environment**
(`secrets … -- bash scripts/kind-smoke/up.sh`, or the dev box's `legion-pane-env`) and are written
only as 0600 files under a 0700 directory that `down.sh` shreds — never on an argv, never printed.

| variable | default | meaning |
| :--- | :--- | :--- |
| `SMOKE_INSTANCE` | `LEGION_ISSUE` lowercased, else `USER`; letters and digits only, 1–9 characters | names everything: cluster `legion-smoke-<instance>`, containers `legion-smoke-<instance>-{nats,postgres}`, tmux server `-L legion-smoke-<instance>`, state dir, Dispatch project `S<INSTANCE>` |
| `SMOKE_DIR` | `${XDG_STATE_HOME:-~/.local/state}/legion-smoke/<instance>` | the state directory |
| `SMOKE_PORT_BASE` | `31000` | NATS `+0`, Envoy listener `+1`, Dispatch `+2`, Postgres `+3`, daemon port-forward `+4`; each checked free before anything starts. A rerun with a base that differs from the instance's recorded one is refused while anything of the instance is live — a container, a recorded process, or the controller's tmux session — naming them and `run scripts/kind-smoke/down.sh first`; with nothing live the ports are re-derived |
| `SMOKE_WORKER_IMAGE` | — (required) | `ghcr.io/sjawhar/legion-worker@sha256:<64 hex>`; a tag is refused |
| `SMOKE_REPO` | `sjawhar/legion-smoke` | the sandbox repository the daemon may drive |
| `SMOKE_ROOT_ISSUES` | `1` | root issues created and released to `todo` |
| `SMOKE_WORKER_CAP` | `6` | the daemon's `worker_cap` |
| `SMOKE_SESSION_STORE` | `pvc` | `pvc` or `postgres` (`postgres` needs a checkout and image that carry `runtime.kubernetes.session_store`, pull request #1108) |
| `SMOKE_GITHUB_INGRESS` | `none` | `none`, or `envoy`: a read-only bridge of `SMOKE_REPO`'s GitHub subjects from `SMOKE_UPSTREAM_NATS` into the instance NATS |
| `SMOKE_UPSTREAM_NATS` | `nats://envoy-nats.tailb86685.ts.net:4222` | the production NATS the bridge subscribes on (never publishes to) |
| `SMOKE_RESYNC_INTERVAL` | `60` | the daemon's `resync_interval_seconds`; the resync probe is what resurrects a crashed root |
| `SMOKE_WORKER_IDLE_RETIRE` | `600` (the daemon's default) | the daemon's `worker_idle_retire_seconds`: how long a finished worker's pod lingers idle; a `worker-cap` run sets it short (the recipe uses 60) so idle pods stop hiding the running count |
| `SMOKE_KILL_ROLE` | `architect` | the pod `kill-pod-resume` crashes; `architect` is the only supported value (see Checkpoints) |
| `SMOKE_LEGION_177_WORKAROUND` | `1` | `1`: run the LEGION-177 keeper and the one-shot unset before the kill; `0`: neither (the close rule below) |
| `SMOKE_LEGION_177_INTERVAL` | `3` | seconds between the keeper's passes |
| `SMOKE_OMP_PROFILE` | `legion-smoke-<instance>` | the isolated OMP profile that `up.sh` initializes for the host daemon, controller, and plugin-skew install; it must name this instance and `down.sh` removes it |
| `SMOKE_OMP_LAUNCH_PREFIX` | `secrets ANTHROPIC_API_KEY GEMINI_API_KEY OPENAI_API_KEY --` | the host daemon's controller-only `omp_launch_prefix`; host mode requires a nonempty prefix, such as the dev-box `legion-pane-env` wrapper |
| `SMOKE_KIND_NODE_IMAGE` | kind's default for its version | `kind create cluster --image` |
| `SMOKE_PROBE_WAIT` | `600` | seconds to wait for the daemon's image probe to pass |
| `SMOKE_POLL_INTERVAL` | `5` | seconds between polls (`up.sh` and the checkpoints) |
| `SMOKE_WAIT_ADMITTED` / `SMOKE_WAIT_ARCHITECT_POD` / `SMOKE_WAIT_SPEC_POSTED` / `SMOKE_WAIT_TREE_MOVED` | `180` / `600` / `1200` / `900` | checkpoint budgets, seconds |
| `SMOKE_WAIT_KILL_PHASE` / `SMOKE_WAIT_KILL_RESUME` / `SMOKE_WAIT_KILL_COMPLETE` | `3600` / `600` / `1800` | `kill-pod-resume`: waiting for an early, active implementer turn, for the replacement (one resync interval plus a pod start), and for the tree to move afterwards |
| `SMOKE_WAIT_CAP_QUEUE` / `SMOKE_WAIT_CAP_PROMOTE` | `1800` / `1800` | `worker-cap`: the queue to fill, the head to be promoted (with cap 1 the promotion waits for the first worker's idle-retire, 600 s) |
| `SMOKE_WAIT_DONE` | `7200` | `done` |
| `LEGION_IMPLEMENT_APP_ID` / `LEGION_REVIEW_APP_ID` | `3202636` / `3202653` | the two GitHub App ids |
| `SMOKE_IMPLEMENT_APP_KEY_FILE` / `SMOKE_REVIEW_APP_KEY_FILE` | — | PEM paths for the two App private keys, the dev-box form (below); the base64 variables win when set |
| `SMOKE_STOP_AFTER` | — | harness only: `up.sh` exits 0 after `host-services`, `overlay`, `daemon`, or `controller` |
| `SMOKE_CONTROLLER_EXAMPLE`, `SMOKE_PLUGIN_MANIFEST` | — | harness only: override the controller example path and the plugin manifest read by the controller decision |

| secret | how it is used |
| :--- | :--- |
| `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, `OPENAI_API_KEY` | at least one is required; the set ones go into the providers Secret (every pod's `/var/run/legion/providers`) |
| `GH_AGENT_APP_PRIVATE_KEY_B64` | the implement App's private key, PEM base64-encoded (the `secrets` form); or `SMOKE_IMPLEMENT_APP_KEY_FILE` |
| `GH_REVIEW_APP_PRIVATE_KEY_B64` | the review App's private key, likewise; or `SMOKE_REVIEW_APP_KEY_FILE` |

The rig generates the rest itself: the Dispatch agent token, the Envoy listener token, the Postgres
password, and — when the checkout's overlay carries `secrets/operator.env.example` — the operator
token.

## Finding a digest

```sh
# 1. The newest cli-v* release body carries the image digest (public API, no gh, no token):
curl -fsSL 'https://api.github.com/repos/sjawhar/legion/releases?per_page=20' \
  | jq -r '.[] | select(.tag_name | startswith("cli-v")) | .body' \
  | grep -oE 'ghcr.io/sjawhar/legion-worker@sha256:[0-9a-f]{64}' | head -n1
# 2. The digest behind a tag (sha-<12 hex of a main commit>, or a CLI version), from the public registry:
tag=sha-<12>; token=$(curl -fsS 'https://ghcr.io/token?scope=repository:sjawhar/legion-worker:pull' | jq -r .token)
curl -fsSI -H "Authorization: Bearer $token" \
  -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
  "https://ghcr.io/v2/sjawhar/legion-worker/manifests/$tag" | grep -i docker-content-digest
# 3. With docker's imagetools (read-only inspection, not a build):
docker buildx imagetools inspect ghcr.io/sjawhar/legion-worker:$tag --format '{{json .Manifest.Digest}}'
```

The image's source commit is its `org.opencontainers.image.revision` label (the config blob behind
the manifest, or `docker buildx imagetools inspect <ref> --format '{{json .Image.Config.Labels}}'`).

## Host-daemon mode

`SMOKE_DAEMON_MODE=host` keeps the worker image and every Legion worker in kind, but runs the
daemon on the machine that launched the rig. It creates a two-node cluster; the worker node is
labelled and tainted `legion.dev/pool=legion:NoSchedule`, and the daemon uses an exec-plugin
kubeconfig that mints the `legion-daemon` ServiceAccount token. The daemon records its process,
state directory, controller tmux server `legion-smoke-<instance>`, and providers Secret; `down.sh`
removes only those recorded resources. Multiple instances use distinct Legion IDs and controller
servers as well as distinct ports and containers.
Before every host-mode run, prove the drivers are based on the current pilot rather than a stale
parallel revision:

```sh
jj git fetch --remote origin
jj rebase -b legion/LEGION-19-drivers -d legion/LEGION-19-k8s-pilot@origin
jj log -r 'legion/LEGION-19-k8s-pilot@origin & ancestors(legion/LEGION-19-drivers)' --no-graph
jj log -r 'legion/LEGION-19-k8s-pilot@origin..legion/LEGION-19-drivers' --no-graph
```

The first command must print the pilot head; the second must list only driver commits. Record both
outputs with the smoke evidence before selecting its worker image.

On the Legion dev box, run the host shape as uid `legion`:

```sh
SMOKE_DAEMON_MODE=host SMOKE_IMPLEMENT_APP_KEY_FILE=/etc/legion/implementer.pem SMOKE_REVIEW_APP_KEY_FILE=/etc/legion/reviewer.pem SMOKE_OMP_LAUNCH_PREFIX=/home/legion/.local/bin/legion-pane-env /home/legion/.local/bin/legion-pane-env bash scripts/kind-smoke/up.sh
```

Its port map is NATS `base+0`, Envoy listener `base+1`, Dispatch `base+2`, Postgres `base+3`,
host daemon API `base+4`, and the reverse-dial worker stream `base+5`. The host daemon's
controller is in `tmux -L legion-smoke-<instance>`; `scripts/kind-smoke/daemon-ctl.sh stop|start`
operates only on this host-mode daemon.

Run the lifecycle-sensitive checkpoints in this order:

```sh
for c in admitted architect-pod spec-posted tree-moved kill-pod-resume scheduling controller-pane exec-auth; do
  bash scripts/kind-smoke/checkpoints.sh "$c"
done
bash scripts/kind-smoke/checkpoints.sh exec-auth --wait-refresh
SMOKE_PLUGIN_TGZ=/path/to/version-bumped-pi-legion-envoy.tgz bash scripts/kind-smoke/checkpoints.sh plugin-skew
bash scripts/kind-smoke/checkpoints.sh volume-lost
bash scripts/kind-smoke/checkpoints.sh pod-hygiene
bash scripts/kind-smoke/checkpoints.sh done
```

`SMOKE_EXEC_TOKEN_TTL=2m` shortens the ServiceAccount token only for an `exec-auth
--wait-refresh` proof. `plugin-skew` requires a tarball whose plugin version differs from every
inherited live process before restart-time reconnection can retire an already-finished worker.
`volume-lost` removes every tree pod and the PVC, verifies fresh recovery for the root and every
prior worker, then records each replacement's pod start time and timestamped `workspace-init` log.

## Running it

```sh
cd -- "$LEGION_WORKSPACE"    # or the checkout root
export SMOKE_WORKER_IMAGE=ghcr.io/sjawhar/legion-worker@sha256:<digest>
# On a box with the secrets CLI:
secrets ANTHROPIC_API_KEY GH_AGENT_APP_PRIVATE_KEY_B64 GH_REVIEW_APP_PRIVATE_KEY_B64 -- bash scripts/kind-smoke/up.sh
# On the Legion dev box (no secrets CLI; provider keys come from /etc/legion/provider.env, the App keys are PEM files):
SMOKE_IMPLEMENT_APP_KEY_FILE=/etc/legion/implementer.pem SMOKE_REVIEW_APP_KEY_FILE=/etc/legion/reviewer.pem \
  SMOKE_OMP_LAUNCH_PREFIX=/home/legion/.local/bin/legion-pane-env /home/legion/.local/bin/legion-pane-env bash scripts/kind-smoke/up.sh
# If go is a mise tool rather than on PATH, prefix either line with: mise x go@1.26 --
for c in admitted architect-pod spec-posted tree-moved kill-pod-resume pod-hygiene done; do bash scripts/kind-smoke/checkpoints.sh "$c"; done
bash scripts/kind-smoke/down.sh
# The worker-cap checkpoint needs its own instance:
SMOKE_INSTANCE=<instance>b SMOKE_PORT_BASE=31100 SMOKE_ROOT_ISSUES=2 SMOKE_WORKER_CAP=1 SMOKE_WORKER_IDLE_RETIRE=60 <the same up.sh line>
SMOKE_INSTANCE=<instance>b bash scripts/kind-smoke/checkpoints.sh worker-cap
SMOKE_INSTANCE=<instance>b bash scripts/kind-smoke/down.sh
# Absence checks after down.sh:
kind get clusters; docker ps -a --filter label=legion-smoke.instance=<instance>; tmux -L legion-smoke-<instance> ls; grep -c legion-smoke ~/.kube/config 2>/dev/null
```

`up.sh` is idempotent: a rerun finds the cluster, containers, processes, project, and root issues
and prints `REUSED …` for each; a changed checkout regenerates the overlay and re-applies it. It
ends with one block:

```
KIND SMOKE READY
instance:        legion26
state dir:       /home/legion/.local/state/legion-smoke/legion26
cluster:         legion-smoke-legion26 (kubeconfig: …/kubeconfig; kubectl --kubeconfig …/kubeconfig -n legion get pods)
gateway:         172.30.0.1
nats:            nats://172.30.0.1:31000 (container legion-smoke-legion26-nats)
listener:        http://172.30.0.1:31001 (pid …)
dispatch:        http://172.30.0.1:31002 (pid …; project SLEGION26; login smoke)
postgres:        172.30.0.1:31003 (container legion-smoke-legion26-postgres)
daemon:          http://127.0.0.1:31004 → svc/legion-daemon-demo:13370 (port-forward pgid …)
image:           ghcr.io/sjawhar/legion-worker@sha256:… (daemon API contract 5)
session store:   pvc
worker cap:      6
controller:      none (the checkout has no legion controller start (pull request #1110))
github ingress:  none (checkpoint done will report SKIPPED-BLOCKED)
legion-177:      keeper (pgid …, every 3s; LEGION-177 workaround)
root issues:     SLEGION26-1
records:         /home/legion/.local/state/legion-smoke/legion26/records
```

After `tree-moved`, `kill-pod-resume` waits for an active implementer before crashing the root.
The replacement then arrives within one resync interval plus a pod start. `pod-hygiene` checks the
resulting pods, and `done` proves the controller-driven tree completed.

**Run the checkpoint loop right after `up.sh`, and `kill-pod-resume` straight after `tree-moved`.**
It selects only a root with a Running, ready-confirmed architect pod while the root issue's active
phase is an implementer whose matching claim is ready-confirmed and Running. The implementer must
have started less than 120 seconds earlier and its shim log must show an unfinished turn with a tool
call. Planner phases are ineligible. This kills early in the longer implementation phase, keeping
the dead-root window small relative to that phase. The completion-into-a-dying-root race in
`dispatch://LEGION-182` is therefore excluded by construction; a phase that is not eligible keeps
the checkpoint polling rather than creating an ambiguous result.

## Modes and degradations

- **`controller: none (<reason>)`** — three ordered checks decide whether a controller pane opens:
  the checkout has `legion controller start` and `deploy/kubernetes/daemon/controller.yaml.example`
  (pull request #1110); `POST /legion/v1/controller/secret` with a deliberately wrong bearer answers
  403 (present) rather than 404 (the image predates the route) — nothing is minted, no incumbent's
  secret is revoked; the profile plugin's `legion.daemonApiVersion` equals the image daemon's contract
  (read from its probe log line). Any failing check records `none: <reason>`, `admitted` still passes
  with `controller=none`, and `done` prints `SKIPPED-BLOCKED`.
- **`SMOKE_GITHUB_INGRESS=none`** — no GitHub event reaches the instance, so no pull request is
  ever merged and `done` prints `SKIPPED-BLOCKED`; every other checkpoint runs.
- **`worker-cap`** prints `SKIPPED-BLOCKED` naming `SMOKE_ROOT_ISSUES` and `SMOKE_WORKER_CAP`
  unless the run was started with exactly `2` and `1`.
- **`SMOKE_KILL_ROLE`** other than `architect` prints `SKIPPED-BLOCKED`: a crashed phase worker is
  only retired until something addresses it, so in a run without GitHub ingress a killed tester
  would never return; the daemon resurrects a crashed root by itself.
- **LEGION-177** — on the image's git 2.47, the `credential.interactive=false` provisioning writes
  into the tree's shared clone makes the *next* pod's init container fail its `jj git fetch`
  (`unable to get password from user`): every phase worker and every resurrected root, not only the
  first replacement (the 2026-09-15 run saw the planner fail six generations in a row). With
  `SMOKE_LEGION_177_WORKAROUND=1` (the default) `up.sh` starts a recorded host loop,
  `legion-177-keeper`, that every `SMOKE_LEGION_177_INTERVAL` seconds runs
  `git --git-dir=/legion/repos/github.com/<owner>/<repo>/.git config --unset credential.interactive`
  through `kubectl exec` in each Running Legion pod of the instance (through the instance kubeconfig
  only), logging each pod and time it acted to `logs/legion-177-keeper.log`; `kill-pod-resume`
  applies the same one-shot unset right before the kill and prints `WORKAROUND LEGION-177 applied`,
  and its OK detail says whether the keeper was running.

## Names, ports, records

| what | name / port / file |
| :--- | :--- |
| kind cluster | `legion-smoke-<instance>`; kubeconfig `<state>/kubeconfig` (never `~/.kube/config`) |
| containers | `legion-smoke-<instance>-nats` (`nats:2.10 -js`, gateway:`base+0`), `legion-smoke-<instance>-postgres` (`postgres:16`, gateway:`base+3`), both labelled `legion-smoke.instance=<instance>` |
| host processes | `listener` (gateway:`base+1`), `dispatch` (gateway:`base+2`), `daemon` (host:`base+4`, host mode), `port-forward` (127.0.0.1:`base+4` → `svc/legion-daemon-demo:13370`, cluster mode), `envoy-bridge` (envoy mode), `legion-177-keeper` (a process group); each `<state>/pids/<name>.{pid,start}` + `<state>/logs/<name>.log` |
| controller | tmux server `-L legion-smoke-<instance>`, session `controller`; `<state>/controller/{controller.yaml,operator-token,envoy-token,dispatch-token,instructions.md}` |
| Dispatch | project `S<INSTANCE>`, human login `smoke` (header identity), the scratch server's own `HOME` at `<state>/dispatch-home` |
| overlay | `<state>/overlay` (the filled copy of `deploy/kubernetes/daemon/overlays/kind`), `<state>/base` (the base copied beside it); the render (`kubectl kustomize`, which carries every secret base64-encoded) is validated through a pipe and never written to disk — `kubectl apply -k` renders it again itself |
| secrets | `<state>/secrets/{dispatch-token,envoy-token,postgres-password,postgres.env,operator-token,*-auth-header}`, `<state>/overlay/secrets/{providers.env,operator.env,github-app-*.pem}`, `<state>/controller/{operator,envoy,dispatch}-token`, `<state>/dispatch-home/.local/share/dispatch/signing-key` — 0600 under 0700, every one shredded by `down.sh`; `up.test.sh` runs `up.sh` then `down.sh` against the fakes (whose kustomize emits a `kind: Secret`) and refutes any secret value anywhere under the state directory afterwards |
| records (`<state>/records/`) | `instance`, `port-base`, `image`, `repo`, `github-ingress`, `session-store`, `worker-cap`, `root-issue-count`, `resync-interval`, `worker-idle-retire`, `project` (`smoke-<instance>`), `dispatch-project`, `dispatch-login`, `gateway`, `cluster`, `kubeconfig`, `nats-container`, `postgres-container`, `root-issues` (one key per line), `controller` (`tmux <server> <window>` or `none: <reason>`), `probe-contract`, `legion-177-workaround` (`keeper` or `off`), `profiles.json` (the resources/role_profiles the generated `legion.yaml` carries) |
In host mode, the first successful `controller-pane` check records its tmux server, window, and
`logs/controller-pane.log` in `records/controller-pane-capture`, then pipes the live controller
pane into that retained log. The capture makes any later controller exit visible without changing
the pane or the daemon.

Inside the cluster the base manifests' `demo` names stay (`legion-daemon-demo`,
`legion-demo-providers`, …): the cluster itself is the instance. Host services bind the kind docker
network's IPv4 gateway (chosen by regex from `docker network inspect kind`; on some boxes the IPv6
entry comes first), which both the pods and the host reach.

## Checkpoints

Each prints exactly one `CHECKPOINT <name> OK: <detail>` (exit 0), `CHECKPOINT <name> FAILED:
<reason>` (exit 1, the last observation when a wait ran out), or `CHECKPOINT <name>
SKIPPED-BLOCKED: <what the run lacks>` (exit 3, decided before any network call). `kill-pod-resume`
additionally prints one `WORKAROUND LEGION-177 …` line. Checkpoints read redacted state through the
daemon endpoint, Dispatch through the scratch server, and pods through the instance kubeconfig. The
kill target alone reads the recorded daemon `state.json` for its active-phase metadata; it never
prints that durable state.

| checkpoint | needs | assertion | budget |
| :--- | :--- | :--- | :--- |
| `admitted` | — | the first root is `todo` or later in Dispatch; `trees[<KEY>]` exists; with a controller pane, `controllerLocator.external` | `SMOKE_WAIT_ADMITTED` |
| `architect-pod` | — | the root is `in_progress`, its tree `active`, its locator a Kubernetes pod locator; the pod is Running with `legion.dev/{project,role,generation,tree,issue}` matching state (raw keys); the claim is Bound and mounted | `SMOKE_WAIT_ARCHITECT_POD` |
| `spec-posted` | — | the root has a primary document; no gate registered and no open approval ask (the gate is off — either is FAILED at once); a child exists or a phase worker is claimed on the root | `SMOKE_WAIT_SPEC_POSTED` |
| `tree-moved` | — | a sub-architect on a child, or a phase worker on the root, holds a claim whose pod is Running; no child is a tree or admission entry of its own (LEGION-57, FAILED at once) | `SMOKE_WAIT_TREE_MOVED` |
| `kill-pod-resume` | a root architect plus an early active implementer turn | waits for the root and matching implementer claims to be ready-confirmed and Running, the implementer phase to be under 120 seconds old, and its shim log to show an unfinished tool call; planner phases are ineligible. It records the root session and tree session file, applies the LEGION-177 unset, crashes the root through the kind node (or forced-delete fallback), then proves a same-session replacement at the next generation resumed the recorded file and the tree moved afterwards | `SMOKE_WAIT_KILL_PHASE` / `SMOKE_WAIT_KILL_RESUME` / `SMOKE_WAIT_KILL_COMPLETE` |
| `pod-hygiene` | pods running | the daemon Deployment and pod carry no `legion.dev/project`; every Legion pod's `worker` and `workspace-init` containers carry exactly its profile's requests and limits (from `records/profiles.json`, quantities normalised); no container's `env`, `command`, or `args` in the namespace contains a secret value the rig wrote (compared by value, named by variable, never printed); PID 1 of every Running Legion pod carries no provider key or secret value | single pass |
| `worker-cap` | `SMOKE_ROOT_ISSUES=2 SMOKE_WORKER_CAP=1` | the daemon's worker queue holds a task while at least one phase-worker or sub-architect pod runs (the daemon judged its cap reached); the head is promoted once a runner finishes (it leaves the queue and gets a Pending/Running pod); and worker pods (root architects and pods being deleted excluded) never exceed the cap for longer than `worker_idle_retire_seconds` + 30 s. A pod count is not the daemon's running count: the cap bounds running-or-prompted workers, a finished worker's pod stays alive idle until the daemon retires it, and the state page exposes no run state — so a transient excess is idle lingering (reported in the OK detail with the cap, the idle window, the sample interval, the peak, and how long it lasted) and only a sustained one is a violation | `SMOKE_WAIT_CAP_{QUEUE,PROMOTE}` |
| `done` | a controller, `envoy` ingress, a `gh` that can list `SMOKE_REPO`'s pull requests | every root and child is `done` and each has a merged pull request `legion/<KEY>` on `SMOKE_REPO`; `SKIPPED-BLOCKED` when the run has no controller, when `SMOKE_GITHUB_INGRESS=none`, when `gh` is off PATH, or when `gh pr list --repo <SMOKE_REPO> --limit 1` fails before the wait (unauthenticated, rate-limited, offline); a `gh` failure during the wait is a retry naming gh | `SMOKE_WAIT_DONE` |
| `volume-lost` | a successful `kill-pod-resume`; a root and at least one ready-confirmed worker claim on the same tree PVC | records the root and every ready worker identity, deletes every tree pod and the now-unmounted PVC; the root recovers through init exit 3 as a new session with no `--resume` and the recovery prompt, then each recorded worker does likewise without a launch-failure increase. `logs/volume-lost-recovery.log` records every replacement's start time and timestamped init output | `SMOKE_WAIT_VOLUME_LOST` |
## Teardown

`down.sh` acts only on the records under the instance's state directory and verifies ownership
before every destructive step: the recorded tmux server (`has-session` before `kill-server`), each
recorded process by pid **and** `/proc/<pid>/stat` start ticks (a reused pid is never signalled;
the record is dropped), the recorded cluster name (`kind get clusters` lists it before
`kind delete cluster`), each recorded container by its `legion-smoke.instance` label (a different
label is refused and reported, exit 1 at the end). It shreds the fixed list of secret files and the
kubeconfig; records and logs stay for inspection. A directory with no `records/instance` prints
`<state dir> has no instance record: this directory never started a kind smoke; stopping nothing,
deleting nothing` and exits 0. Afterwards `kind get clusters`, `docker ps -a`,
`tmux -L legion-smoke-<instance> ls`, and `~/.kube/config` carry nothing of the instance.

## The harnesses

```sh
bash scripts/kind-smoke/up.test.sh          # refusals, records, reuse, overlay, crash-loop, controller decision, bridge, summary
bash scripts/kind-smoke/down.test.sh        # the refusal, teardown by record and ownership, the label refusal
bash scripts/kind-smoke/checkpoints.test.sh # every checkpoint's OK, FAILED, and SKIPPED-BLOCKED lines
bun test scripts/kind-smoke/envoy-bridge.test.ts
```

Every external binary is a PATH fake that logs its argv; the harnesses need only bash, coreutils,
jq, and `ss`, and CI runs them in the `test` job (and `bun test scripts/kind-smoke` for the bridge, its own step). They also assert that no secret value ever
reaches an argv or the output. The assertion rules live in `test-lib.sh`: under `set -e` bash
exempts a `!`-inverted command and every operand of an `&&` list but the last from errexit
(shellcheck SC2251), so a bare `! grep …` can never fail a harness — every negative assertion is
`refute CMD…` (CMD succeeding fails the harness naming the line), every positive one is a single
bare command on its own line, and the ERR trap names the file, line, and command of any failure.

## Troubleshooting

See `docs/kubernetes.md`, "Runbook: the kind smoke", for the troubleshooting table.
