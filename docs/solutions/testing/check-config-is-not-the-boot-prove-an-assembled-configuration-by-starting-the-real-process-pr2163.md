---
title: "--check-config is not the boot: prove an assembled configuration by starting the real process"
category: testing
tags:
  - legion-yaml
  - boot
  - live-proof
date: 2026-11-02
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2163"
---

# --check-config is not the boot: prove an assembled configuration by starting the real process

- `legion start --check-config` validates what the file says.
  The boot also refuses on values it resolves from outside the file (a GitHub App's bot login, a token, a disk), which the checker never reads.

- Before you claim a configuration works, or plan a proof around one, start the real daemon on it as far as the environment lets, and quote where it stopped.

- When you add a refusal over resolved values, say in its comment that `--check-config` cannot reach it.

## Evidence

Round 27 tried to run the parked-decision devbox proof with one GitHub App in both `github_apps` slots.
`legion start --check-config` printed `Config OK`;
the real boot passed the plugin gate and the store, config and Dispatch stages, then exited:
`the review App names bot login "legion-implementer[bot]" and the implement App "legion-implementer[bot]"; the workflow needs two different Apps`.
That refusal is `appLoginsAtBoot` (`packages/daemon/internal/daemon/workflow.go:214`, the comparison at `:230`), which reads each App's login from GitHub under its own JWT, so no config-time check can see it.
The round changed no code; the plan had cost a round because only the boot could answer it.
