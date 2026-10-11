---
title: "What the worker image owns and what the deployment owns: an image ENV is a variable the operator's pod env is refused, and a cold pull is a figure only the cluster can time"
category: daemon
tags:
  - worker-image
  - dockerfile
  - operator-pod-env
  - imageEnv
  - PUPPETEER_EXECUTABLE_PATH
  - image-size
  - registration-deadline
  - stage-4a
date: 2026-10-08
status: active
module: packages/daemon/docker, packages/daemon/internal/runtime/sandbox
related_issues:
  - "LEGION-578"
  - "sjawhar/legion#1846"
---

# What the worker image owns and what the deployment owns

- Every variable the image's final `ENV` sets is one `runtime.kubernetes.pod.env` is refused
  (`internal/runtime/sandbox/operatorpod.go` `imageEnv`, held to the Dockerfile by
  `TestImageEnvIsWhatTheWorkerImageSets`). A knob the operator may legitimately turn — a browser
  path, a model setting — must therefore not be an image `ENV`: make the image satisfy the tool's
  own default resolution (a `chromium` on PATH, which Oh My Pi finds before any download) and let
  the probe check whatever the operator's pod env names instead.
- A probe that runs the operator's value is the negative control's hook: a pod env naming an
  executable that does not run is refused by name
  (`image-probe-capability-negative`), which is what the operator should learn at boot rather
  than at a worker's first use.
- Before and after an image change, record the compressed size from the registry (the index's
  amd64 manifest, config plus layers) and the bound a cold pull must fit (the registration deadline,
  `worker_boot_timeout_seconds × worker_boot_registration_deadline_intervals`). Do not estimate the
  pull time: no pod can time a cold-node pull of its own image, so the figure is Stage 4a's, run by
  the operator lane on the pull request's digest, and the handoff names that run as needed.

## Evidence

sjawhar/legion#1846 first set `ENV PUPPETEER_EXECUTABLE_PATH=/usr/bin/chromium` so Oh My Pi's
browser would be the image's; `TestImageEnvIsWhatTheWorkerImageSets` failed, since the variable
would then be refused in every operator's pod env. The ENV came out: Oh My Pi 18.6.0 resolves
`PUPPETEER_EXECUTABLE_PATH` first, else `google-chrome-stable`, `google-chrome`, `chromium`,
`chromium-browser`, `chrome` on PATH, before any download (read from the pinned bundle), so the
image's `/usr/bin/chromium` is found with nothing set, and the probe's browser row runs the same
resolution — Stage 4a's new `image-probe-capability-negative` sets
`PUPPETEER_EXECUTABLE_PATH=/nonexistent/chromium` in the operator pod and expects the refusal
`capability browser is missing`.

The image grew from 12 layers and 386,510,448 bytes compressed (1.18.1) to 22 layers and
1,056,282,603 bytes, measured from ghcr.io's manifests (Chromium's library closure 275 MiB of it,
the Go toolchain 64 MiB, gopls 20 MiB, Node's tree with the two language servers 66 MiB).
`docs/kubernetes.md` records the sizes and the 360 s bound and says the pull time is Stage 4a's;
the operator lane ran it at c88a3502 on the pull request's digest and the pod registered inside a
51 s checkpoint (dispatch://LEGION-578/comment/474f51f1-8d82-467a-ae65-e862a6128a50).
