---
title: "A pod rehearses a checkpoint's own readers over its session and probes the release rootfs under unshare before asking for a four-hour run"
category: legion
tags:
  - live-proof
  - stage4b
  - coordinator-lane
  - pre-flight
  - gvisor
  - probe-image
date: 2026-10-16
status: active
module: skills/legion-worker
related_issues:
  - "LEGION-578"
  - "sjawhar/legion#1866"
---

# A pod rehearses a checkpoint's own readers over its session and probes the release rootfs under unshare before asking for a four-hour run

- An issue pod's kubectl creates nothing (`kubectl auth can-i create sandboxes.agents.x-k8s.io
  -n legion` → `no`), so no agent of a tree runs Stage 4a or 4b. The run is the coordinator lane's
  (the Legion PO's session, live holder of Envoy role `legion-deploy`), asked by `envoy_send` with
  one message carrying everything: the head and that it contains `main`, the image by digest and
  the `Worker Image` run that published it, both commands with every operator input a placeholder,
  the expected verdict lines and the checkpoint→criterion map, the re-run entry points, the rule
  that the lane fixes nothing in the checkout, and what to paste back and where. A full Stage 4b is
  3.5–4 hours and the lane runs one at a time; a failing checkpoint costs the lead-in before it,
  then the fix, a new image (~15 min) and the whole run again.
- Before the request, rehearse in the pod every assertion a pod can reach, with the checkpoint's
  own readers over your own session: run the checkpoint's instruction against yourself, then lift
  the script's helpers (`sed -n '/^session_jq() {/,/^}/p; …'`) and run them over your session file
  (`/legion/sessions/<cwd slug>/<timestamp>_<session id>.jsonl`, the subagent's beside it at
  `<stem>/<task name>.jsonl`), including every negative control. A reader that fails on your own
  session will fail on the run's.
- Rehearse the interpreter too: fetch the jq versions the runner may use into `/tmp` and run the
  e2e library's tests under each; read upstream defaults from the pinned binary, never from memory
  (`a-settings-proof-asserts-the-relation-…`). The image carries no jq binary — the harness bash
  offers one as a builtin — so bun's child processes and the e2e tests need a fetched jq on `PATH`
  (LEGION-764 adds one to the image).
- Probe the release image's rootfs in the pod when the proof's criterion is the image: pull the
  layers by digest from `ghcr.io` with the anonymous pull token, extract them in order honouring
  whiteouts, and run the image's own `legion probe-image … --pod-safety` with `unshare -r -m
  --root=<rootfs> --mount-proc=/proc --wd=/home/legion env -i <the image's PATH/HOME/OMP_PROFILE/
  LEGION_OMP_PATH>`. There is no `chroot` binary in the image, and gVisor lets a user namespace
  neither `mknod` a device nor bind-mount a file, so a regular file at `<rootfs>/dev/null` stands
  in for `/dev/null` (Go opens it, writes land in the file). Remove `usr/local/bin/gopls` for the
  control: `capability lsp: missing (gopls is not on PATH)`, exit 1.
- The daemon's Postgres-backed tests need a database the pod lacks: zonky's embedded Postgres
  binaries (a Maven jar holding a `.txz`; extract with Python's `lzma`+`tarfile`, since the image
  has no `xz`) give an `initdb`/`pg_ctl` that runs as the pod user on a high loopback port in
  under five seconds; `LEGION_TEST_PG_DSN=postgres://postgres@127.0.0.1:<port>/postgres`. The
  testcontainers-backed cases still need Docker and are skipped by name in the pod, green in CI.
- When a review changes how a checkpoint judges (its comparand, its binding, a guard) and not what
  it drives (its instructions, its kills, its fixtures), the architect may rule a narrower
  re-proof: the module's arms in a unit, the exact runtime read driven once against the real daemon
  from the pod (`GET $LEGION_DAEMON_URL/legion/v1/state`, no credential), the tightened readers
  replayed over the kept real sessions with a negative that the old form accepted — recorded as a
  plan departure. A change on the driving side is a new run.

## Evidence

sjawhar/legion#1866 (LEGION-578), generation 3: the implementer rehearsed `full-agent-tools`
against itself before the first request (12 `tool_result_said` PASS, the subagent's verdict
`{login: true, yield: true, other: false}`, both controls), and the architect and planner
answered the same list over Envoy as `legion-reviewer[bot]`; the live run later passed that
checkpoint first time. What was not rehearsed cost a run each: the runner's jq 1.8.2 (the pod's
builtin and CI's jq compiled the library), the pinned Oh My Pi's defaults (pinned as literals), and
`main`'s hold loop under host load; a fourth run was a host `/dev/pts` outage the lane diagnosed
itself. Five Stage 4b runs and ~16 hours of the lane's time in all; the fifth passed every
checkpoint (`stage 4b e2e: PASS`, 13,294 s). The release image `sha256:c69e8c5d…` was probed in
the pod's extracted rootfs (`probe-image: OK (/opt/omp/bin/omp) session-storage=probed
extensions=discovered agent-models=skipped capabilities=checked model-fallback=on
daemon-api-version=24`, 4.8 s) with the `gopls` control, and the admit package's 20
Postgres-backed tests ran against embedded Postgres 16.15 in the pod. Round 2's narrower re-proof
(architect's Envoy d0540c3d33d7a601f0f1e396158823df) replayed the kept sessions and read the
production daemon's state once, with no sixth run.
