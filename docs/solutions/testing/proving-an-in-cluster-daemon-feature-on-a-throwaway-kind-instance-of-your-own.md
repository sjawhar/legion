---
title: "Proving an in-cluster daemon feature on a throwaway kind instance of your own: the standing order's correction, the branch image by digest, a short resync, one relayed root event, an inert controller, and a teardown that leaves nothing"
category: testing
tags:
  - kind
  - kubernetes-runtime
  - throwaway-instance
  - worker-image
  - workflow_dispatch
  - digest-pinned
  - resync_interval_seconds
  - LEGSMOKE
  - cross-admission
  - deployment-instructions
  - operator-token
  - standing-order
  - teardown
date: 2026-09-15
status: active
module: deploy/kubernetes/daemon, packages/daemon/src/daemon/runtime-kubernetes.ts, .github/workflows/worker-image.yaml
related_issues:
  - "LEGION-25"
  - "sjawhar/legion#1110"
  - "LEGION-81"
  - "sjawhar/legion#1108"
symptoms:
  - "the criterion is reached only through an in-cluster daemon (a Kubernetes-runtime feature) and the pull request's E2E line links a unit suite"
  - "the pull request cannot point at a Worker Image of its own head: main's image predates the branch, the probe pod refuses the branch's daemon API contract"
  - "in a ten-minute proof the daemon never logs the controller-bound line you are waiting for"
  - "your rig's daemon grows trees for LEGSMOKE issues you never created"
applies_when:
  - Proving a `runtime: kubernetes` daemon change before READY (spec acceptance that names a kind check)
  - Deciding what a worker may stand up on the shared dev box after the 2026-09-13 order and its 2026-09-14 correction
  - Building the worker image for an unmerged branch
---

# Proving an in-cluster daemon feature on a throwaway kind instance of your own

## The order and its correction

Sami's standing order of 2026-09-13 (19:00Z, via the operator) withdrew every pre-merge rig:
no smoke rig, no scratch daemon, no throwaway NATS or tmux server, no throwaway OMP profile —
[pre-merge-surface-withdrawn-by-standing-order](../legion/pre-merge-surface-withdrawn-by-standing-order-what-the-pr-carries-instead.md)
records how a pull request carries its proof when the surface is withdrawn. On 2026-09-14 at
19:41Z Sami corrected it (LEGION-25 spec v21): a worker **may** build a throwaway instance of
its own — its own cluster, containers, ports, state directory, tmux server, and OMP profile —
and **must** tear it down; what stays forbidden is the shared dev-box daemon and its state, the
operator's `~/.omp/profiles/legion` and `~/.config/legion/*`, another issue's rig, and any pane
the worker did not start. Under the correction a reachable surface is not "withdrawn": for a
Kubernetes-runtime change the kind check is required before READY, the implementer runs it on
an instance of their own, and the tester runs it again on a *separate* instance of their own
(LEGION-25: `legion-25b` and `legion-25t`, reproducing observation for observation). Ask the
architect which order applies before writing "withdrawn"; do not assume the 09-13 text.

## The recipe (what cost time to learn)

**Names and ports are yours.** Suffix everything with your issue: kind cluster `legion-<n>x`,
NATS container `legion-<n>x-nats` on a port nobody else uses, the Envoy listener on the kind
gateway address (`172.30.0.1:<port>`, so pods reach it) with `ENVOY_API_TOKEN` set and no
`ENVOY_API_ALLOW_UNAUTHENTICATED`, `tmux -L legion-<n>x`, `OMP_PROFILE=legion<n>-test` with the
plugin packed from the branch, a state directory under `/tmp/legion-<n>x-rig`, a port-forward
port of your own. Own binaries too (`kind`, `kubectl` under `/tmp/legion-<n>x-bin`) — do not
install into a shared PATH. `kind get clusters` before and after must show only other issues'
clusters.

**The image is the branch's, by digest.** `legion gh -- workflow run worker-image.yaml --ref
legion/<KEY>` builds the branch head; off `main` the workflow publishes
`ghcr.io/sjawhar/legion-worker:sha-<short>` **only** — `tag_release` is `true` only on
`refs/heads/main` with a `cli_version`, so no `cli-v*` release body is edited, and the ECR
mirror step skips every non-main run. Read the digest from the run's summary and pin it in *both*
places — the overlay's kustomize `images:` and `runtime.kubernetes.image` in the overlay's
`legion.yaml`. The probe pod is your first assertion: its log must read `probe-image: OK (…)
… daemon-api-version=<the branch's LEGION_DAEMON_API_VERSION>`; with the branch's contract
bumped, main's image fails that line by design (`plugin-daemon-api-contract-version-gate`).
Note the code head the image was built from in the proof: a later conflict-forced rebase
carries the same code under a new SHA, and the record has to say which one the pod ran.

**Shorten the resync so controller-bound wakes are observable.** The daemon's controller-bound
log lines and wakes ride the resync tick: `controller not registered; run legion controller
start` is throttled to once per `worker_boot_timeout_seconds`, and the forced resync at
`/controller/ready` is what publishes the `triage` notice for an untriaged root. With the
default `resync_interval_seconds: 600` a ten-minute proof sees one tick or none. Set `60` in
the overlay's `legion.yaml`; then at `worker_boot_timeout_seconds: 120` the not-registered line
lands every second tick and its absence on the ticks between is itself an observation.

**Relay exactly one event: your own root's.** Never subscribe the rig to
`notifications.dispatch.issue.>`: every LEGSMOKE rig shares the project prefix, and a daemon
admits any issue whose `issue.created` it saw once it moves to `todo` — the cross-admission
incident in
[two-envoy-rigs-on-the-shared-legsmoke-project](../legion/two-envoy-rigs-on-the-shared-legsmoke-project-cross-admit-and-a-branch-behind-mains-plugin-contract-gets-its-own-profile.md).
Create your own LEGSMOKE root, read its real `issue.created` from Dispatch, and `POST
/v1/messages/publish` it to your listener once. Your daemon then has exactly one issue to
triage, and no other rig sees anything from you.

**Make the controller inert through the rig's deployment instructions.** The controller you
start is a real Oh My Pi with the real skill; on the shared LEGSMOKE project it would triage,
move statuses, and start trees. Point the overlay's `instructions:` at a file that tells it to
render every notice (`NOTICE RECEIVED: <json>`) and act on none — then the `triage` notice
reaching the terminal is the observation, and no LEGSMOKE status changes. Set your throwaway
root `done` at teardown.

**Secrets travel as files and leave with the rig.** The two App PEMs may be read from
`/etc/legion/implementer.pem` and `/etc/legion/reviewer.pem`; copy them 0600 into the overlay
copy's gitignored `secrets/` (0700) beside `providers.env`, and generate a random
`OPERATOR_TOKEN` there too (`operator.env`). Never commit, log, or paste one. The controller's
operator token on your side is a 0600 file `legion controller start` refuses when group- or
world-readable — the refusal is a free negative control.

**Controls that belong in the record.** A wrong operator token (a second random file): exit 1
naming the URL and `403 Invalid operator token`, the daemon's one `refused POST
/legion/v1/controller/secret: wrong operator token` line, the incumbent's record and Envoy claim
unchanged, no state directory written. A closed port-forward (`--daemon-url` at a dead port):
exit 1 naming the URL, one attempt, no fallback. The controller terminal closed (`/exit`): the
command's exit is Oh My Pi's, the listener answers 404 for the role within seconds, the record
stays in `legion state` until the next `/controller/ready` replaces it (last claim wins), and
the not-registered line resumes on its throttle.

**Teardown is part of the proof.** `kind delete cluster --name legion-<n>x`; `docker rm -f
legion-<n>x-nats`; kill your listener; `tmux -L legion-<n>x kill-server`; `shred -u` every PEM
copy and token file; `rm -rf` the state directory, the overlay copy, and
`~/.omp/profiles/legion<n>-test`; set the LEGSMOKE root `done`; `kind get clusters` and `docker
ps` show only what was there before. Record the teardown time in the proof — the tester and the
reviewer check that the instance no longer exists.

## What the record looks like

`proof[]` in the implement handoff and the PR's `E2E (implementer)` line each carry: the image
digest and the `Worker Image` run id, the code head it was built from, the apply and rollout
result, the probe-pod line, the throttled log timestamps, the `controllerLocator` and role
claim as `legion state` and the listener's `GET /v1/roles/<controller token>` show them, the
notice rendered in the terminal with its event id, the three controls, and the teardown. Attach
the long form as an issue artifact (`dispatch_artifact`, e.g. `kind-run-part-b.md`) and link it
from one PR comment, so the PR body stays a summary.

## Related

- [proof-without-the-rig-what-the-daemon-suite-and-ci-caught-and-the-one-gap-they-left](../legion/proof-without-the-rig-what-the-daemon-suite-and-ci-caught-and-the-one-gap-they-left.md)
  — what the unit suite and CI prove when no instance can reach the surface.
- [scratch-daemon-rig-proves-what-unit-tests-cannot](scratch-daemon-rig-proves-what-unit-tests-cannot.md)
  — the tmux-runtime equivalent of this instance.
- [dispatch-playwright-harness-from-a-legion-pane-unset-envoy-url-and-own-your-ports-database-and-paths](dispatch-playwright-harness-from-a-legion-pane-unset-envoy-url-and-own-your-ports-database-and-paths.md)
  — the same own-your-ports rule for the Dispatch UI harness.
