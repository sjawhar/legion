---
title: "Two envoy-mode rigs on the shared LEGSMOKE project cross-admit each other's issues; a branch behind main's plugin contract runs its rig on a dedicated OMP profile built from the branch"
category: legion
tags:
  - smoke-rig
  - LEGSMOKE
  - cross-admission
  - SMOKE_WEBHOOK_MODE
  - envoy-bridge
  - OMP_PROFILE
  - pi-legion-envoy
  - daemonApiVersion
  - LEGION_DAEMON_API_VERSION
date: 2026-09-13
status: active
module: scripts/smoke, packages/daemon
related_issues:
  - "LEGION-60"
  - "sjawhar/legion#1030"
  - "LEGION-61"
  - "LEGION-44"
symptoms:
  - "your rig's daemon grows trees for LEGSMOKE issues you never created (their parent is another rig's root)"
  - "another rig's daemon spawns an architect on your root or child and its implementer pushes to your `legion/LEGSMOKE-<n>` branch"
  - "your root issue is moved triage→todo by `legion-daemon:LEGSMOKE` before your own daemon has seen it"
  - "the daemon refuses to start: `pi-legion-envoy … speaks daemon API contract <n>; this daemon requires <m>`"
applies_when:
  - Starting a smoke rig with `SMOKE_WEBHOOK_MODE=envoy` while any other LEGSMOKE rig may be live
  - Running a rig from a branch whose `LEGION_DAEMON_API_VERSION` differs from the plugin installed in the profile you would use
---

# Two envoy-mode rigs on the shared LEGSMOKE project cross-admit each other's issues

## What happened

The LEGION-60 implementer's rig (project `sjawhar/60`, root LEGSMOKE-129, child LEGSMOKE-131) ran
while LEGION-44's tester rig was live on the same box. Within minutes each daemon had admitted the
other's issues as trees of its own: mine grew LEGSMOKE-132/133 (children of their LEGSMOKE-127),
theirs grew architects for my 129 and 131 — and their implementer pushed to *my*
`legion/LEGSMOKE-131` branch and opened a sandbox PR on it. Before I knew whose LEGSMOKE-132 was, I
moved it backlog→done through the API; its owner moved it back.

Why: every rig's `envoy-bridge.ts` subscribes to `notifications.dispatch.issue.>` — **every**
Dispatch issue project's events — and the daemon (`reducers.ts`) admits any issue whose
`issue.created` it saw and that later moves to `todo` (`reduceIssueUpdated` → `admit`). The daemon
filters by project key prefix only; every LEGSMOKE rig shares that prefix. The tester's later rig
saw the other direction too: a rig elsewhere relaying production NATS moved its freshly created
root to `todo` before its own daemon had seen the issue.

This is LEGION-61's problem to solve (the rig has no mode that is both isolated and live). Until
it lands, the operational rules below are what worked.

## Running beside another rig

1. **Check first.** `ps -eo pid,args | grep "cli/index.ts start"` and `docker ps` show every live
   rig daemon and NATS container on the box. If another LEGSMOKE rig is up in `envoy` mode, expect
   cross-admission both ways and coordinate through Envoy with its owner (role topic
   `notifications.role.legion-sjawharlegion-legion-<n>-<role>`).
2. **Prefer `SMOKE_WEBHOOK_MODE=none` plus a hand relay of your own tree's events** over `envoy`
   mode when another rig is live. The tester's round used `/tmp/legion60-test/inject-issue-event.ts`
   and `relay-tree.ts` (LEGION-27's recipe) to relay only its own issues' Dispatch events into the rig
   NATS; nothing outside its tree was touched. In that mode the other direction still bites (the
   other rig sees your events), so cycle your own issue `backlog→todo` with truthful `issue.updated`
   relays if the other rig triaged it first.
3. **If you are already contaminated:** stop your bridge (`kill -TERM` its pid from
   `${SMOKE_DIR}/envoy-bridge.pid`) so no more foreign events arrive, stop the daemon, excise the
   foreign trees from `state.json` (`trees`, `issues`, `admission.active/queue`,
   `workerAdmission.queue`, their `roles`, `phases`, `gates`), kill their tmux windows, and restart
   in `none` mode. Do not PATCH another rig's issue status; message its owner instead.
4. **Give every rig its own everything:** `SMOKE_DIR`, `SMOKE_PROJECT`, `NATS_PORT`, `ENVOY_PORT`,
   `LEGION_DAEMON_PORT` (and its `+1` worker-stream port). Since #1010 the NATS container name is
   derived per project (`legion-smoke-nats-<slug>`) and `down.sh` tears down only what
   `${SMOKE_DIR}/legion.yaml` names, so two rigs no longer collide on the container — but ports and
   directories are still yours to separate. `SMOKE_WORKER_CAP=1` makes a task queue behind a full
   cap without editing the generated `legion.yaml`.

## A branch behind main's daemon API contract

The daemon refuses to start unless the installed `@sjawhar/pi-legion-envoy` manifest's
`legion.daemonApiVersion` equals the branch's `LEGION_DAEMON_API_VERSION`
(`verifyLegionPluginContract`; see
`docs/solutions/daemon/plugin-daemon-api-contract-version-gate.md`). When main bumps the contract
under a long-lived branch, the `legion` profile's plugin is ahead of the branch, and rebasing to catch
up is forbidden unless GitHub reports CONFLICTING (Sami, 2026-09-11).

The sanctioned path is a **dedicated OMP profile with the plugin built from the branch**:

```sh
# build the plugin exactly as the release does
cd packages/pi-envoy && bun run build && rm -rf dist/skills && cp -r ../../skills dist/skills
PKG=/tmp/<rig>/pi-legion-envoy && mkdir -p "$PKG" && cp -r dist "$PKG/dist" && cp README.md "$PKG/"
jq --arg v "<version>-<issue>.<sha>" '.version=$v | .omp.extensions=["dist/envoy.js","dist/legion.js"] | del(.scripts,.devDependencies)' package.json > "$PKG/package.json"
# a profile that is the legion profile with only that plugin swapped
P=~/.omp/profiles/<rig>; mkdir -p "$P/agent" "$P/plugins"
cp ~/.omp/profiles/legion/agent/{config.yml,models.yml,keybindings.yml} "$P/agent/"
for l in agents hooks prompts WATCHDOG.md; do ln -s "$(readlink -f ~/.omp/profiles/legion/agent/$l)" "$P/agent/$l"; done
jq --arg f "file:$PKG" '.dependencies["@sjawhar/pi-legion-envoy"]=$f' ~/.omp/profiles/legion/plugins/package.json > "$P/plugins/package.json"
(cd "$P/plugins" && bun install)
```

then start the rig with `OMP_PROFILE=<rig> PI_PROFILE=<rig>` in the daemon's environment **and**
`SMOKE_OMP_LAUNCH_PREFIX="env OMP_PROFILE=<rig> secrets … --"` so every pane the daemon spawns loads
the same profile (the daemon resolves the plugin root through OMP's `DirResolver`, which reads
`OMP_PROFILE`). The other profile plugins (secretsd, knives, superpowers, codegraph) come along
unchanged, which is what makes this the production-like plugin tree the deployment rules require —
never the Legion extension loaded alone. Remove the profile when the rig is torn down; its OMP
session transcripts go with it, so copy any you need as evidence first (this retro's implementer
lost two that way).

When the contract is *equal* after a rebase — as it was for the tester's round — no dedicated
profile is needed; check `jq .legion.daemonApiVersion ~/.omp/profiles/legion/plugins/node_modules/@sjawhar/pi-legion-envoy/package.json`
against `packages/contracts/src/legion-daemon-api.ts` before deciding.

## Scrub the pane environment before starting a rig from a worker pane

A Legion phase worker's own shell carries `LEGION_*`, `TMUX*`, `JJ_CONFIG`, `GIT_CONFIG_*`, `GH_*`,
`PI_CODING_AGENT_DIR`, and `OMP_SESSION_ID`. `up.sh` and the daemon strip the secret family, but a
rig started from inside a pane still inherits the rest; start it with `env -u` for each of those
(the tester's launcher dropped `LEGION_DAEMON_PORT` by the same scrub and took the default port —
check the port you meant is the port you got). `DISPATCH_TOKEN` must come from
`DISPATCH_TOKEN_FILE` via `bash -c 'DISPATCH_TOKEN="$(cat "$0")" … exec bash scripts/smoke/up.sh' "$DISPATCH_TOKEN_FILE"`,
never pasted.
