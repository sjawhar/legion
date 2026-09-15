---
title: "Prove a daemon-only Kubernetes change before an image exists: the branch's daemon on the host against pods from the published image, a node-side kill -9, and the ledger that shows nobody intervened"
category: testing
tags:
  - kind
  - kubernetes-runtime
  - throwaway-instance
  - host-daemon
  - published-image
  - crictl
  - kill-pod
  - negative-control
  - spawnRequests
  - env-i
  - kind-smoke
date: 2026-09-15
status: active
module: packages/daemon (runtime-kubernetes.ts, the host `legion start` with runtime.kubernetes.kubeconfig), scripts/kind-smoke (LEGION-26's rig, SMOKE_STOP_AFTER=overlay), docs/kubernetes.md
related_issues:
  - "LEGION-179"
  - "sjawhar/legion#1122"
  - "LEGION-19"
  - "LEGION-26"
  - "LEGION-177"
---

# Prove a daemon-only Kubernetes change before an image exists: the branch's daemon on the host against pods from the published image, a node-side kill -9, and the ledger that shows nobody intervened

## The gate

Under `runtime: kubernetes` the in-cluster daemon **is** the worker image, and the Worker Image
workflow builds only from `main` (or by hand); its pull-request trigger fires for
`packages/daemon/docker/**`, `omp-pin.ts`, and the workflow file, not for `processes.ts`. So an
acceptance line of the form "on a kind instance, kill the tester pod and watch the daemon
relaunch it" cannot be run in the literal in-cluster shape before the branch merges: no image
carries the branch's daemon. Reporting "pending an image" is a report to the architect, not a
proof — and it was not necessary.

## The route: host daemon from the branch, pods from the published image

The Kubernetes runtime's own supported host shape proves a daemon-only change on real pods:

- `legion.yaml`: `runtime: { kubernetes: { namespace, image: <published digest>, kubeconfig:
  <the kind kubeconfig> } }`, `bind: 0.0.0.0`, `daemon_url: http://<kind gateway>:<port>`
  (the docker `kind` network's IPv4 gateway — pods reach the host through the node), a
  `worker_stream_port` the pods can dial, `envoy_url`/`nats_urls`/`dispatch_url` at the same
  gateway, `state_dir` under the instance's scratch root. `resync_interval_seconds: 60` so the
  backstop is observable within the run.
- Start it from the issue workspace: `bun packages/daemon/src/cli/index.ts start <project>
  --config <that file>`. `--check-config` first; the run needs `DISPATCH_TOKEN` in the
  environment when `dispatch_url` is set.
- The pods run the published image unchanged. The change under test is on the host: the death
  path, the probe, the relaunch decision. The pod side (shim, OMP, the extension's boot
  handshake) is exactly what production runs.

LEGION-26's `scripts/kind-smoke` rig (on its branch until it lands) stands up everything but the
daemon in one command with `SMOKE_STOP_AFTER=overlay`: the kind cluster, the instance's NATS and
Postgres containers, the Envoy listener and a scratch Dispatch server bound at the gateway on the
instance's ports, the scratch project mapped to the sandbox repository, and the rendered overlay.
From that overlay apply only the `Namespace` and the providers `Secret` to the cluster — never
the daemon `Deployment` — then create the root issue in the scratch project and release it to
`todo`. Tear down with the rig's `down.sh` (it stops and deletes by record only) plus your own
host daemon and any loop you started; `shred -u` the rendered overlay and any file you split the
providers Secret into — the rig's shred list does not know about them.

Two things a worker in a Legion pane must do that the rig cannot:

- **Start the daemon under `env -i`.** The pane carries the dev-box daemon's `LEGION_*`,
  `DISPATCH_*`, and `ENVOY_*` variables; a scratch daemon that inherits them hands them to its
  probe pod and every worker pod. Pass exactly `PATH`, `HOME`, `USER`, `LANG`, `TERM`, your own
  `XDG_STATE_HOME`/`XDG_DATA_HOME` (so the legions registry under `~/.local/state/legion` is
  never touched), and `DISPATCH_TOKEN`.
- **Keep the LEGION-177 workaround running** until an image carries LEGION-178: a loop that
  `kubectl exec`s `git --git-dir=/legion/repos/github.com/<owner>/<repo>/.git config --unset
  credential.interactive` in every Running Legion pod, or the second generation's init fetch
  fails with `unable to get password from user`. Record whether it was on.

The first pipeline ran architect → planner → implementer → tester in about four minutes on this
box; run the kill the moment the target role's claim shows `readyConfirmedAt` and its pod
`Running` — a kill during a phase transition lands on an idle worker and proves the wrong thing.

## Kill the process from the node, never delete the pod gracefully

A graceful `kubectl delete pod` sends the shim its shutdown and OMP exits cleanly: that exercises
the stop path, not a crash. And `kubectl exec <pod> -- kill -9 1` does nothing — the kernel drops
a `SIGKILL` sent to pid 1 from inside its own pid namespace. Crash it from the node:

```sh
node=$(kind get nodes --name <cluster>)
cid=$(docker exec "$node" crictl ps -q --name worker --label io.kubernetes.pod.name=<pod>)
pid=$(docker exec "$node" crictl inspect --output go-template --template '{{.info.pid}}' "$cid")
docker exec "$node" kill -9 "$pid"
```

`kubectl delete pod --grace-period=0 --force` is the fallback when the node is not reachable
that way. Both make the shim's stream close, which is the observation the daemon acts on first;
the resync probe is the backstop and, on a healthy stream, never gets there first.

## What to read as evidence

- The daemon log: `<role token>: worker process died (its stream closed and the one reconnect
  was refused); launch failure 1/3; relaunching the same agent with --resume and its catch-up`
  and `respawning <issue> by resuming OMP session <path>` — quote both.
- The replacement pod: `kubectl get pod <issue>-<role>-g<n+1> -o json` — its container
  **`command`** (not `args`, which is empty) carries `--resume=<that path>`; its env carries
  `LEGION_GENERATION=<n+1>`.
- The claim on `GET /legion/v1/state`: generation `n+1`, the **same** `sessionId` (the same-agent
  invariant), `readyConfirmedAt` set again, `launchFailures` gone (reset by the confirmed ready).
- The phase completing: the Dispatch status transition and the Envoy listener's `listener
  received` / `listener role forwarded` lines for the `phase-complete` payload on the architect's
  role topic, naming the architect's session.
- **That nobody intervened:** the redacted state page does not expose `pendingAssignment` or the
  spawn ledger, so read the daemon's durable `state.json` under its `state_dir`:
  `spawnRequests` holds one entry per `spawn_worker`; exactly one for the killed role, settled
  before the kill, proves the relaunch was the daemon's and not the architect's.

## The negative controls

Two, both cheap:

- **A live pod probed alive is untouched.** Every resync tick logs `resync probed N active roots
  and M confirmed workers`; the other roles' claims and pods stay at their generation across
  those ticks.
- **A finished worker is retired, not relaunched.** Kill the pod of a role whose phase has
  completed (the planner, idle after its phase, with `phases[issue]` naming a later role) the
  same way: the log reads `… worker process died (…) after finishing: <issue>'s active phase is
  <role> and nothing is queued for this role; retired, not relaunched — only spawn_worker
  resumes a finished worker`, no `-g2` pod appears, the claim keeps its generation with
  `launchFailures` untouched and `resumeSessionFile` kept.

Record the digest, whether the LEGION-177 workaround was on, the kill command with its pid, the
two log lines, the g2 pod's command, the status transitions with timestamps, the ledger count,
and both controls; then tear the instance down. The tester repeats the same recipe on an
instance of its own — the proof is a recipe, not a screenshot.
