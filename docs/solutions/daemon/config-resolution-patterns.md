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
  - "LEGION-13"
  - "sjawhar/legion#978"
  - "LEGION-17"
  - "sjawhar/legion#956"
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

## Pattern 6: CLI env injection with `process.env` at the citty boundary only

The config-resolving CLI commands take their environment as a parameter and thread it down to
`resolveDaemonConfig({ env, … })`, which already accepted `opts.env`:

```typescript
function loadStartConfig(project, configPath, env: NodeJS.ProcessEnv, options = {}): DaemonConfig
export async function cmdCheckConfig(project, configPath, env: NodeJS.ProcessEnv): Promise<void>
async function cmdStart(project, configPath, env: NodeJS.ProcessEnv): Promise<void>
async function cmdRestart(project, configPath, env: NodeJS.ProcessEnv): Promise<void>
```

Only the citty `start` and `restart` handlers name `process.env`. A test hands `cmdCheckConfig` the
environment it wants — `{ PATH: process.env.PATH, HOME: process.env.HOME }` for the fixture shape, or
`{ ...env, DISPATCH_URL, DISPATCH_TOKEN }` to exercise a resolver rule — and never scrubs or restores
`process.env` around a case (`src/cli/__tests__/index.test.ts`, `legion start --check-config`).

Three rules that came out of LEGION-13 (`sjawhar/legion#978`):

- **No `= process.env` default on the injected parameter.** A default lets a call site silently fall back to
  ambient state, which is the exact bug the seam exists to remove: the check-config test had read the pane's
  `DISPATCH_URL` (set) and `DISPATCH_TOKEN` (unset) and failed in every Legion pane while passing in CI.
- **Bare positional `env` for a single injected collaborator; a deps object only when there are several.**
  `cmdGh`, `cmdCredential`, `cmdProbeImage`, and `cmdHandoffComplete` take `{ env, fetch, … }` because they
  inject two or more; `daemonUrl(env)`, `resolveControllerSecret(env)`, and the four commands above take
  `env` alone. Match the count, not the nearest example.
- **Prove the seam with a test only the seam can pass, and revert-guard it in the CI shape.** The
  pre-existing tests pass whether the env is injected or scrubbed; the test that injects `DISPATCH_URL`
  without `DISPATCH_TOKEN` and expects the refusal is the one that fails when `loadStartConfig` goes back to
  `process.env` (see [fix-racing-a-workaround](../legion/fix-racing-a-workaround.md) § 3).

Boundary, deliberately unchanged: `cmdStop`, `cmdStatus`, and `cmdLegions` still call
`resolveLegionPaths(process.env, …)` — they need the legions-registry path, not the daemon configuration.

The sentinel-abort DI this pattern originally described (`cmdStart(undefined, { config }, { startDaemon,
resolveLegionId })`, from #436) no longer matches the code: `cmdStart` calls `startDaemon` directly and has no
deps object.

## Pattern 7: A path-valued key is validated twice — the string at config load, the file at boot before the probes

`instructions` (LEGION-17, `sjawhar/legion#956`) names a markdown file the daemon appends to every
launched pane's system prompt. Two different failures want two different moments:

- **The string, at config load.** `loadConfigFromFile` rejects `""`/whitespace with `requireNonEmpty` and
  resolves a relative value against the config file's directory (`configDir`), exactly like `state_dir`;
  `resolveDaemonConfig` applies the ordinary `resolveValue` order (cli > file > `LEGION_INSTRUCTIONS` >
  absent) and rejects an empty env value naming the env key. An env value is used as given — there is no
  config file to be relative to. `legion start --check-config` therefore validates the *key* without
  opening the file. Precedence is never special-cased per key: the spec's "env override" wording was
  read as "wins over absent", the only reading consistent with every other key (architect ruling; the
  test `resolves instructions: … LEGION_INSTRUCTIONS as given when the file omits it` pins it).
- **The file, at boot, before anything slow.** `startDaemonLocked` calls
  `materializeDeploymentInstructions` right after the Dispatch-token secret write and *before*
  `verifyOmpAgentsCapability`, the plugin contract gate, and the plugin load probe — each of which spawns
  an OMP process and can take seconds, retry, or (on a loaded box) minutes. A missing, unreadable,
  directory, or blank file refuses startup naming the resolved path; a failed write of the materialized
  copy refuses startup too. The boot test asserts the ordering, not just the refusal: with an unreadable
  path, `probed === false`, `loadedState === false`, `natsCreated === false`, and no
  `deployment-instructions.md` exists afterwards (`index.test.ts` › "refuses to boot on an unreadable
  instructions path before loading state").

Rule: for any new key that names a file the daemon consumes, validate the string where the other keys
are validated and read the file at boot ahead of the OMP probes — never lazily at first use, where a
misconfiguration would surface as a pane launch failure hours later — and write the boot test so it
fails if the read is moved after a probe.
