---
title: "Mixed-Runtime Type Boundaries in Bun Monorepos"
category: infra
tags:
  - pulumi
  - tsconfig
  - bun
  - node
  - types
  - mixed-runtime
date: 2026-04-05
status: active
module: envoy-infra
related_issues:
  - "sjawhar-legion-257"
symptoms:
  - "Pulumi ts-node fails with bun-types"
  - "Cannot find module bun:test"
  - "types: bun-types blocks Node.js type discovery"
  - "Pulumi deployment broken after server reboot"
---

# Mixed-Runtime Type Boundaries in Bun Monorepos

## Context

This monorepo uses Bun as its primary runtime. However, some packages run under Node.js — notably `packages/envoy/infra/` which uses Pulumi (ts-node). When Bun-specific type definitions leak into a Node.js package's TypeScript configuration, the package fails to compile under its intended runtime.

## The Problem

A package in the Bun monorepo had `"types": ["bun-types"]` in its tsconfig and `@types/bun` in devDependencies. This caused two issues:

1. **TypeScript's `types` array is exclusive**: when set explicitly, TypeScript ONLY loads the listed `@types/*` packages. This blocked auto-discovery of `@types/node` (needed by Pulumi).
2. **Bun ambient types conflict with Node.js**: `bun-types` provides Bun-specific globals that shadow or conflict with Node.js types.

## The Pattern

When a package in a Bun monorepo uses a non-Bun runtime (Pulumi/ts-node, AWS Lambda, etc.):

### tsconfig.json

1. **Remove `"types": ["bun-types"]`** — let TypeScript auto-discover `@types/*` from node_modules.
2. **Remove `@types/bun` from devDependencies** — prevents Bun types from being auto-discovered when the explicit `types` array is removed.
3. **Exclude `__tests__/` from tsconfig** — test files that use `bun:test` cannot be compiled by ts-node. Add `"__tests__"` to the `exclude` array.

### package.json

Remove `@types/bun` from devDependencies. After removal, run `bun install` to regenerate the lockfile (removes stale entries).

### Documentation

Add a comment in tsconfig or README explaining the runtime choice. Any package with a non-default runtime should be explicit about it.

## Checklist for Mixed-Runtime Packages

- [ ] tsconfig does NOT have `"types": ["bun-types"]`
- [ ] package.json does NOT have `@types/bun` in devDependencies
- [ ] `__tests__/` is excluded from tsconfig if tests use `bun:test`
- [ ] README states which runtime is used and why
- [ ] If secrets are involved, README includes passphrase/env var recovery steps

## Bun Workspace Exclusion

Bun's workspace glob (`"workspaces": ["packages/*"]`) includes ALL packages, including
Node.js-runtime ones. This causes Bun to try to resolve Node.js packages' dependencies
through its own resolver, which can pollute `@types/node` versions and break the daemon's
typecheck.

**Fix:** Switch from glob to explicit list in root `package.json`:

```json
{
  "workspaces": [
    "packages/contracts",
    "packages/daemon",
    "packages/envoy",
    "packages/envoy-plugin",
    "packages/opencode-plugin"
    // packages/aws-infra intentionally excluded — Node.js/ts-node runtime
    // packages/envoy/infra intentionally excluded — Node.js/ts-node runtime
  ]
}
```

Add a comment explaining why the package is excluded. Run `bun install` after changing
the workspace list to regenerate `bun.lock`.

## Anti-Pattern: Copy-Paste from Sibling Packages

The root cause was inheriting Bun-specific config from sibling packages when creating a Node.js-runtime package. When creating a new package that runs under a different runtime, start from a clean tsconfig for that runtime rather than copying from an existing Bun package.
