---
title: "Config Resolution Patterns: Multi-Source Precedence with Deprecation Tracking"
category: daemon
tags:
  - config-schema
  - dependency-injection
  - deprecation
  - testing
  - validation
date: 2026-04-11
status: active
module: daemon
related_issues:
  - "sjawhar-legion-436"
symptoms:
  - "env var deprecation warnings"
  - "config file vs env var precedence"
  - "unknown YAML key warning"
  - "startDaemon signature change broke tests"
---

# Config Resolution Patterns: Multi-Source Precedence with Deprecation Tracking

Learnings from adding YAML config file support to the daemon (#436), replacing scattered `process.env` reads with a layered resolution system.

## Pattern 1: `resolveValue<T>` with Source Tracking

The core abstraction: a generic function that takes values from each source layer and returns both the winning value and which source it came from.

```typescript
type ValueSource = "cli" | "config" | "env" | "default";

function resolveValue<T>(
  cliValue: T | undefined,
  configValue: T | undefined,
  envValue: T | undefined,
  defaultValue: T
): { value: T; source: ValueSource }
```

**Why source tracking matters:** Deprecation warnings should only fire when the env var is the *effective* value — not when it's set but overridden by config or CLI. The `source` field makes this a simple `source === "env"` check. An early draft checked `env[envVar] !== undefined` instead, which incorrectly warned even when the user had migrated to config.

**Gotcha — three places per new field:** Adding a new config field requires updates in (1) `loadConfigFromFile()` for YAML→fields mapping, (2) `resolveDaemonConfig()` for the `resolveValue()` call, and (3) `CONFIG_SCHEMA` for unknown-key detection. Missing any one causes silent bugs or spurious warnings.

## Pattern 2: Two-Phase Config Loading

Separate YAML parsing from precedence resolution:

1. **Phase 1** (`loadConfigFromFile`): Parse YAML → produce `{ fields: Record<string, unknown>, warnings: string[] }`. Validates syntax, enums, path normalization, GitHub App completeness. No knowledge of env vars.
2. **Phase 2** (`resolveDaemonConfig`): Merge `configFile.fields` with env values and CLI overrides using `resolveValue()` for each field. Produces fully typed `DaemonConfig` plus deprecation warnings.

**Why this separation matters:** Each phase is independently testable. CLI tests can inject a fake `configFile` without touching YAML. Config file tests validate parsing without env var setup. The `LoadedConfigFile` type is the clean boundary.

**Note:** `LoadedConfigFile.fields` uses TypeScript-side names (camelCase), not YAML keys. The `loadConfigFromFile()` function handles the snake_case→camelCase mapping.

## Pattern 3: Schema-Driven Unknown-Key Detection

A recursive `CONFIG_SCHEMA` constant defines the known YAML structure:

```typescript
const CONFIG_SCHEMA: ConfigSchema = {
  project: null,        // null = leaf node
  controller: {         // object = nested section
    session_id: null,
    prompt: null,
  },
  // ...
};
```

A `collectUnknownKeys()` walker traverses parsed YAML against this schema and emits dotted-path warnings like `github_apps.impl.extra_field`.

**Why not Zod:** We wanted warnings (non-fatal) for unknown keys, not errors. Zod's `strict()` throws. The schema walker is ~15 lines and produces exactly the UX we need.

**Sync gotcha:** The schema must stay in sync with the parsing logic. If you add a YAML key to `loadConfigFromFile()` but forget `CONFIG_SCHEMA`, users get spurious warnings.

## Pattern 4: Test Helpers After API Signature Changes

Changing `startDaemon()` from `Partial<DaemonConfig>` overrides to a fully resolved `DaemonConfig` broke ~30 tests. The fix: a `buildConfig()` / `startDaemonForTest()` helper that calls `resolveDaemonConfig()` to construct a valid base, then spreads test-specific overrides.

```typescript
function buildConfig(paths, stateFilePath, overrides = {}) {
  const { config } = resolveDaemonConfig({ env: { LEGION_ID: "acme/widgets" } });
  return { ...config, paths, legionId: "acme/widgets", stateFilePath, ...overrides };
}
```

**Lesson:** When changing a function from "partial with defaults" to "fully resolved", plan for a test helper that constructs valid instances. Don't make every test build the full object from scratch — that creates fragile tests that break on every new required field.

## Pattern 5: Validation Parity Across Code Paths

**The bug:** The env-based `loadConfig()` silently accepted `extra_projects` with `backend: linear`. The YAML-based `loadConfigFromFile()` correctly rejected it. Review caught this — the env path was written before the validation requirement existed and was never audited.

**The fix:** Move the validation into `resolveDaemonConfig()` where it applies regardless of source.

**Lesson:** When adding validation to a new code path (YAML parser), always audit existing code paths (env parser) for the same field. Two entry points for the same data → two places that need the same validation. The safest approach: validate in the resolver (Phase 2), not the parser (Phase 1), so all sources get the same treatment.

## Pattern 6: CLI DI with Sentinel Abort

`cmdStart()` accepts a `deps` parameter for `startDaemon` and `resolveLegionId`. Tests inject mocks that capture args and throw a sentinel to abort cleanly:

```typescript
const START_DAEMON_ABORT = "__start-daemon-abort__";
await cmdStart(undefined, { config: configPath }, {
  startDaemon: async (config) => {
    calls.push(config);
    throw new Error(START_DAEMON_ABORT);
  },
  resolveLegionId: async (team) => team,
});
```

This tests CLI wiring without running the actual daemon. Cleaner than module mocking because the DI seam is explicit in the function signature.
