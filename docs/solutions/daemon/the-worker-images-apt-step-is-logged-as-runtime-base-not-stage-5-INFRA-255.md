---
title: "The worker image's apt step is logged as [runtime-base 2/2], not [stage-5 2/22]"
category: daemon
tags:
  - worker-image
  - github-actions
  - buildkit
  - cache
  - proof
date: 2026-10-26
status: active
module: packages/daemon/docker
related_issues:
  - "INFRA-255"
  - "sjawhar/legion#2216"
---

# The worker image's apt step is logged as [runtime-base 2/2], not [stage-5 2/22]

Extends docs/solutions/daemon/a-cached-image-layer-s-output-is-in-the-run-that-built-it-cite-both-runs-LEGION-764.md.

That note's rule stands; its anchor changed. Since sjawhar/legion#2216 the runtime image's apt RUN is
the stage `runtime-base`, and the final stage is `runtime`:

- Find the apt step in a `Worker Image` log as `#<n> [runtime-base 2/2] RUN apt-get …`, then check
  whether the next line is `#<n> CACHED`, as that note says. `stage-5` no longer appears.
- The `Setting up <pkg>` lines that tell before from after are the ones under that step: the
  `runtime-base` stage's, not the final stage's, which installs nothing.

## Evidence

`gh run view 40249247479 --log` (head 4d6d8cb3) shows `#44 [runtime-base 2/2] RUN apt-get update &&
apt-get install -y --no-install-recommends bind9-dnsutils ca-certificates chromium …`.
