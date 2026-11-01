---
title: "Multi-layer service consolidation checklist"
category: envoy
tags:
  - refactoring
  - docker
  - pulumi
  - ci
  - deployment
  - consolidation
date: 2026-04-11
status: active
module: envoy
related_issues:
  - "427"
symptoms:
  - "CI fails after deleting cmd/ directories"
  - "go build ./cmd/github: directory not found"
  - "gh run rerun still fails after fixing workflow"
---

# Multi-layer service consolidation checklist

When consolidating multiple Go binaries into one, all layers must be updated atomically in a single PR. Missing any layer leaves dead code or broken builds.

## The 7 layers

| Layer | What changes | Easy to miss? |
|-------|-------------|---------------|
| **Go code** | Delete `cmd/*/main.go`, extract handlers to `internal/` | No |
| **Dockerfile** | Remove `go build` + `COPY` lines for deleted binaries | No |
| **Compose files** | Delete per-service compose files, add env vars to remaining | No |
| **Pulumi types** | Remove old config interfaces, add new ones to existing config | Somewhat |
| **Pulumi services** | Delete `create*` functions, add inline logic to remaining | Somewhat |
| **CI workflows** | Remove build/pack steps for deleted binaries | **Yes — most missed** |
| **Deploy scripts** | Delete per-service deploy scripts | Somewhat |

## CI workflows are the hidden dependency

CI workflow files (`.github/workflows/*.yaml`) are not referenced by any code and don't surface in `go build`, `tsc --noEmit`, or linter output. They only fail when CI runs after the PR is pushed.

**Check both workflows:**
- PR/CI workflow (`envoy-and-contracts.yaml`) — build + test steps
- Release workflow (`release-envoy-and-plugin.yaml`) — binary pack + artifact steps

**Search pattern:**
```bash
grep -r "cmd/github\|cmd/slack\|cmd/ghostwispr" .github/workflows/
```

## `gh run rerun` doesn't pick up workflow fixes

When CI fails due to a stale workflow and you fix the workflow on the branch:
- `gh run rerun --failed` replays the **original run's commit and workflow file**, not the branch head
- The fix: push a new commit SHA to trigger a fresh CI run
- Don't waste cycles on reruns when the failure is in the workflow file itself

## Config schema migration (Pulumi YAML)

When consolidating services, the Pulumi config schema changes:

```yaml
# Before: separate receiver config
receivers:
  github: true
  slack: true

# After: nested in listener config
listener:
  webhooks:
    github: true
    slack: true
```

This is a manual migration — update `Pulumi.prod.yaml` directly. There's no automated migration tooling for Pulumi stack config schema changes.

## Pulumi `requireSecret` → `getSecret`

When consolidating, secrets that were required per-service may become optional at the Pulumi level (because not all machines need all providers). Change `cfg.requireSecret()` to `cfg.getSecret()` and let the Go binary validate required secrets at startup via config-gating.

## Deployment migration: parallel operation window

For webhook URL changes at external providers (GitHub settings, Slack app config):

1. Deploy the consolidated listener with webhook routes enabled
2. Old receivers continue serving on their original ports
3. Update webhook URLs at providers to point to the listener
4. Verify delivery on the new endpoint
5. Remove old receiver containers

Dedupe keys are source-derived (`github.<delivery_id>`, `slack.<event_id>`), so duplicate delivery during the overlap window is safe — NATS deduplication handles it.
