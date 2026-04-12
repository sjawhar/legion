# OMO Replacement: Workflow Matrix Contract Tests (T1)

**Issue:** #270 | **Part of:** #200 (Replace oh-my-opencode with opencode-legion)

## Context

This task creates a single contract test file that proves every OMO-dependent capability is
currently absent or incomplete in `packages/opencode-plugin`. ALL tests must FAIL on first run —
that's the point. They become the acceptance gate for subsequent tasks (T6, T8, T10, T11, T16-T21).

**Key constraint:** Tests must fail with meaningful errors (not "cannot find module"). We achieve
this by importing from existing modules and asserting that specific named exports or behaviors
are absent.

## Files

| File | Action | Purpose |
|------|--------|---------|
| `packages/opencode-plugin/src/__tests__/omo-replacement-matrix.test.ts` | **CREATE** | Contract test matrix — all 7 OMO capability areas |

No other files are created or modified.

## Design Decisions (from Metis pre-analysis)

- **Area 1 (Skill system):** Assert named exports (`discoverSkills`, `parseSkill`, `injectSkill`,
  `createSkillMcpManager`) don't exist on the plugin index. These will fail until skill system is
  implemented.
- **Area 2 (Spawn limits):** Assert `BackgroundTaskManager` has no `getDepth()` or
  `getDescendantCount()` method, and that `LaunchOptions` has no `maxDepth` field. These are
  structural gaps — tests fail until depth tracking is added.
- **Area 3 (Agents):** Assert OMO role names (Atlas, Hephaestus, Prometheus, Athena) are NOT
  present in `createAgents()` output. Tests will fail once a mapping function is added that
  exposes these names.
- **Area 4 (Tool guards):** Assert `createWriteExistingFileGuard` doesn't exist on the hooks
  index. The stop-continuation-guard is a placeholder — assert its `tool.execute.before` hook
  is absent (it's not wired yet).
- **Area 5 (Context management):** Assert compaction threshold is NOT configurable — the
  `createPreemptiveCompactionHook` doesn't accept a `threshold` option. Tests fail until
  configurable threshold is implemented.
- **Area 6 (Model fallback):** Assert `createModelFallbackChain` doesn't exist on the overlays
  index. Simple named-export check — fails until fallback chain is implemented.
- **Area 7 (Code quality):** Assert `noUncheckedIndexedAccess` and `exactOptionalPropertyTypes`
  are NOT in tsconfig (stricter options not yet enabled). Tests fail until quality gates are
  tightened.

## Task 1: Create the contract test file

**File:** `packages/opencode-plugin/src/__tests__/omo-replacement-matrix.test.ts`

Create this file with the following content:

```typescript
/**
 * OMO Replacement Matrix — Contract Tests (T1)
 *
 * Part of #200: Replace oh-my-opencode (OMO) with opencode-legion.
 *
 * ALL tests in this file MUST FAIL on initial run. They define the acceptance
 * gate for subsequent tasks (T6, T8, T10, T11, T16-T21). A passing test here
 * means either (a) the feature was already implemented, or (b) the test is wrong.
 *
 * Test strategy: Import from existing modules and assert that specific named
 * exports or behaviors are absent. This produces meaningful failures (not
 * "cannot find module") while proving the gaps.
 *
 * Source of OMO concept names: oh-my-opencode documentation and architecture
 * assessment at docs/solutions/architecture-patterns/plugin-migration-assessment.md
 */

import { describe, expect, it } from "bun:test";
import tsconfig from "../../tsconfig.json";
import { createAgents } from "../agents";
import { BackgroundTaskManager } from "../delegation";
import * as delegationIndex from "../delegation";
import * as overlaysIndex from "../overlays";
import * as pluginIndex from "../index";

// ─── Area 1: Skill System ────────────────────────────────────────────────────
// OMO gates MCP tools behind skills — tools only load when the skill is active.
// opencode-legion has no skill system yet (confirmed: no "skill" keyword in src/).
// These tests assert the missing exports that the skill system must provide.

describe("Area 1: Skill system", () => {
  it("discoverSkills is not yet exported from plugin index", () => {
    // Will fail once skill discovery is implemented
    expect((pluginIndex as Record<string, unknown>)["discoverSkills"]).toBeDefined();
  });

  it("parseSkill is not yet exported from plugin index", () => {
    // Will fail once skill parsing is implemented
    expect((pluginIndex as Record<string, unknown>)["parseSkill"]).toBeDefined();
  });

  it("createSkillMcpManager is not yet exported from plugin index", () => {
    // Will fail once skill-scoped MCP manager is implemented
    // Key architectural requirement: tools keyed by (sessionID, skillName, serverName)
    expect((pluginIndex as Record<string, unknown>)["createSkillMcpManager"]).toBeDefined();
  });

  it("injectSkill is not yet exported from plugin index", () => {
    // Will fail once skill injection into session context is implemented
    expect((pluginIndex as Record<string, unknown>)["injectSkill"]).toBeDefined();
  });
});

// ─── Area 2: Spawn Limits ────────────────────────────────────────────────────
// OMO tracks spawn depth and descendant budget to prevent runaway delegation.
// BackgroundTaskManager has no limit enforcement (confirmed: no limit/depth/budget
// in background-manager.ts). LaunchOptions has no maxDepth field.

describe("Area 2: Spawn limits", () => {
  it("BackgroundTaskManager has no getDepth method", () => {
    // Will fail once depth tracking is added to BackgroundTaskManager
    const proto = BackgroundTaskManager.prototype as Record<string, unknown>;
    expect(proto["getDepth"]).toBeDefined();
  });

  it("BackgroundTaskManager has no getDescendantCount method", () => {
    // Will fail once descendant budget tracking is implemented
    const proto = BackgroundTaskManager.prototype as Record<string, unknown>;
    expect(proto["getDescendantCount"]).toBeDefined();
  });

  it("LaunchOptions type has no maxDepth field (structural gap)", () => {
    // LaunchOptions currently: { agent, prompt, description, parentSessionId?, model?,
    // systemPrompt?, timeoutMs? }. No maxDepth.
    // This test asserts the gap by checking the delegation index exports no maxDepth schema.
    // Will fail once LaunchOptions gains a maxDepth field and it's exported.
    expect((delegationIndex as Record<string, unknown>)["MAX_SPAWN_DEPTH"]).toBeDefined();
  });

  it("BackgroundTaskManager has no enforceSpawnLimits method", () => {
    // Will fail once concurrency + depth limits are enforced at launch time
    const proto = BackgroundTaskManager.prototype as Record<string, unknown>;
    expect(proto["enforceSpawnLimits"]).toBeDefined();
  });
});

// ─── Area 3: Agents ──────────────────────────────────────────────────────────
// OMO defines 4 named roles: Atlas, Hephaestus, Prometheus, Athena.
// opencode-legion has 10 agents with different names (orchestrator, executor, etc.).
// These tests assert the OMO role names SHOULD be present (as aliases or mappings)
// but currently are NOT — so they fail now and pass once the mapping is implemented.
// Pattern: toContain("atlas") FAILS now (absent), PASSES when mapping is added.

describe("Area 3: Agents (OMO role mapping)", () => {
  const agents = createAgents();
  const agentNames = agents.map((a) => a.name);

  it("Atlas agent role alias is not yet present in createAgents() output", () => {
    // Fails now: "atlas" not in agent names. Passes when Atlas→orchestrator alias is added.
    expect(agentNames).toContain("atlas");
  });

  it("Hephaestus agent role alias is not yet present in createAgents() output", () => {
    // Fails now: "hephaestus" not in agent names. Passes when Hephaestus→executor alias is added.
    expect(agentNames).toContain("hephaestus");
  });

  it("Prometheus agent role alias is not yet present in createAgents() output", () => {
    // Fails now: "prometheus" not in agent names. Passes when Prometheus→explorer alias is added.
    expect(agentNames).toContain("prometheus");
  });

  it("Athena agent role alias is not yet present in createAgents() output", () => {
    // Fails now: "athena" not in agent names. Passes when Athena→oracle alias is added.
    expect(agentNames).toContain("athena");
  });
});

// ─── Area 4: Tool Guards ─────────────────────────────────────────────────────
// OMO has a write-existing-file guard that blocks writes to files not yet read.
// opencode-legion has stop-continuation-guard (placeholder) and subagent-question-blocker,
// but NO write-existing-file guard.

describe("Area 4: Tool guards", () => {
  it("createWriteExistingFileGuard is not yet exported from plugin index", () => {
    // Will fail once write-existing-file guard is implemented
    expect((pluginIndex as Record<string, unknown>)["createWriteExistingFileGuard"]).toBeDefined();
  });

  it("stop-continuation-guard tool.execute.before hook is not yet wired", async () => {
    const { createStopContinuationGuardHook } = await import("../hooks/stop-continuation-guard");
    const guard = createStopContinuationGuardHook();
    expect(guard["tool.execute.before"]).toBeDefined();
  });

  it("write guard tracks which files have been read per session", () => {
    // Will fail once write guard maintains a per-session read-file registry
    expect((pluginIndex as Record<string, unknown>)["createReadFileRegistry"]).toBeDefined();
  });
});

// ─── Area 5: Context Management ──────────────────────────────────────────────
// OMO has a configurable context window monitor. opencode-legion has preemptive-compaction
// with a HARDCODED 0.78 threshold. The missing piece: configurable threshold.

describe("Area 5: Context management", () => {
  it("createPreemptiveCompactionHook does not accept a threshold option", async () => {
    const { createPreemptiveCompactionHook } = await import("../hooks/preemptive-compaction");
    expect(createPreemptiveCompactionHook.length).toBeGreaterThanOrEqual(2);
  });

  it("compaction threshold is not configurable via plugin config", () => {
    // PluginConfig has no compactionThreshold field.
    // Will fail once config schema gains a compactionThreshold option.
    const { loadPluginConfig } = require("../config");
    // The config schema doesn't validate compactionThreshold — passing it should be ignored
    // (passthrough) rather than recognized. Once recognized, this test fails.
    expect((pluginIndex as Record<string, unknown>)["CONFIGURABLE_COMPACTION_THRESHOLD"]).toBeDefined();
  });

  it("compaction state preservation captures tool call history", async () => {
    const { COMPACTION_CONTEXT_TEMPLATE } = await import("../hooks/compaction-context-injector");
    expect(COMPACTION_CONTEXT_TEMPLATE).toContain("Tool Call History");
  });
});

// ─── Area 6: Model Fallback ──────────────────────────────────────────────────
// OMO has chain-based model fallback (if primary fails, try next in chain).
// opencode-legion has getModelOverlay() for per-provider system prompts but NO
// fallback chain or retry-with-different-model logic.

describe("Area 6: Model fallback", () => {
  it("createModelFallbackChain is not yet exported from overlays", () => {
    // Will fail once chain-based fallback is implemented
    expect((overlaysIndex as Record<string, unknown>)["createModelFallbackChain"]).toBeDefined();
  });

  it("getModelOverlay does not support fallback chain configuration", () => {
    // Currently: getModelOverlay(providerID, modelID) → ModelOverlay | null
    // Will fail once it accepts a fallback chain: getModelOverlay(providerID, modelID, fallbacks?)
    expect(overlaysIndex.getModelOverlay.length).toBeGreaterThanOrEqual(3);
  });

  it("BackgroundTaskManager has no retry-with-fallback-model behavior", () => {
    // LaunchOptions has no fallbackModels field.
    // Will fail once retry logic with model fallback is implemented.
    expect((delegationIndex as Record<string, unknown>)["createRetryWithFallback"]).toBeDefined();
  });
});

// ─── Area 7: Code Quality ────────────────────────────────────────────────────
// OMO enforces strict TypeScript and Biome. opencode-legion has strict: true and
// Biome, but is missing stricter options that OMO requires.

describe("Area 7: Code quality (stricter gates)", () => {
  it("tsconfig does not yet enable noUncheckedIndexedAccess", () => {
    // Will fail once noUncheckedIndexedAccess is added to tsconfig
    expect(tsconfig.compilerOptions).toHaveProperty("noUncheckedIndexedAccess", true);
  });

  it("tsconfig does not yet enable exactOptionalPropertyTypes", () => {
    // Will fail once exactOptionalPropertyTypes is added to tsconfig
    expect(tsconfig.compilerOptions).toHaveProperty("exactOptionalPropertyTypes", true);
  });
});
```

**Verify the test count:**
```bash
grep -c "^\s*it(" packages/opencode-plugin/src/__tests__/omo-replacement-matrix.test.ts
# Expected: 18 (≥15 required, ≥2 per area)
```

**Area breakdown:**
- Area 1 (Skill system): 4 tests
- Area 2 (Spawn limits): 4 tests
- Area 3 (Agents): 4 tests
- Area 4 (Tool guards): 3 tests
- Area 5 (Context management): 3 tests
- Area 6 (Model fallback): 3 tests
- Area 7 (Code quality): 2 tests
- **Total: 23 tests** (well above ≥15)

## Task 2: Run the tests and verify all fail

```bash
cd packages/opencode-plugin
bun test src/__tests__/omo-replacement-matrix.test.ts
```

**Expected output:** Non-zero exit code. All 23 tests fail. Failure messages must be meaningful
(not "cannot find module"). Example expected failures:

```
✗ discoverSkills is not yet exported from plugin index
  expect(received).toBeDefined()
  Received: undefined

✗ Atlas agent role is not yet mapped
  expect(received).toContain("atlas")
  Received: ["orchestrator", "executor", "oracle", "explorer", "librarian", "metis", "momus", "multimodal", "conductor", "simplicity-reviewer"]

✗ tsconfig does not yet enable noUncheckedIndexedAccess
  expect(received).toHaveProperty("noUncheckedIndexedAccess", true)
  Received: {"target": "ESNext", "strict": true, ...}
```

**If any test PASSES:** That test is wrong — either the feature already exists (update the test
to assert the missing piece) or the assertion is trivially true. Fix before proceeding.

## Task 3: Fix any accidentally-passing tests

**Definition of "accidentally passing":** A test exits green (0 failures) on the FIRST run,
before any implementation changes are made. This means the feature already exists or the
assertion is trivially true.

All 23 tests and their expected failure reasons:

| Test | Expected failure reason |
|------|------------------------|
| discoverSkills not exported | `undefined` fails `.toBeDefined()` |
| parseSkill not exported | `undefined` fails `.toBeDefined()` |
| createSkillMcpManager not exported | `undefined` fails `.toBeDefined()` |
| injectSkill not exported | `undefined` fails `.toBeDefined()` |
| BackgroundTaskManager has no getDepth | `undefined` fails `.toBeDefined()` |
| BackgroundTaskManager has no getDescendantCount | `undefined` fails `.toBeDefined()` |
| MAX_SPAWN_DEPTH not exported | `undefined` fails `.toBeDefined()` |
| BackgroundTaskManager has no enforceSpawnLimits | `undefined` fails `.toBeDefined()` |
| Atlas alias not in createAgents() | `["orchestrator", ...]` doesn't contain `"atlas"` |
| Hephaestus alias not in createAgents() | `["orchestrator", ...]` doesn't contain `"hephaestus"` |
| Prometheus alias not in createAgents() | `["orchestrator", ...]` doesn't contain `"prometheus"` |
| Athena alias not in createAgents() | `["orchestrator", ...]` doesn't contain `"athena"` |
| createWriteExistingFileGuard not exported | `undefined` fails `.toBeDefined()` |
| stop-continuation-guard tool.execute.before not wired | `undefined` fails `.toBeDefined()` |
| createReadFileRegistry not exported | `undefined` fails `.toBeDefined()` |
| createPreemptiveCompactionHook.length < 2 | function.length is 1, fails `>= 2` |
| CONFIGURABLE_COMPACTION_THRESHOLD not exported | `undefined` fails `.toBeDefined()` |
| COMPACTION_CONTEXT_TEMPLATE missing "Tool Call History" | string doesn't contain that text |
| createModelFallbackChain not exported | `undefined` fails `.toBeDefined()` |
| getModelOverlay.length < 3 | function.length is 2, fails `>= 3` |
| createRetryWithFallback not exported | `undefined` fails `.toBeDefined()` |
| noUncheckedIndexedAccess not in tsconfig | property absent, fails `.toHaveProperty()` |
| exactOptionalPropertyTypes not in tsconfig | property absent, fails `.toHaveProperty()` |

If a test passes unexpectedly:
1. Verify the feature is actually absent by checking the source file
2. If the feature exists: rewrite the test to assert the NEXT missing piece
3. If the assertion is trivially true: tighten it to assert the actual gap

Common pitfalls:
- `getModelOverlay` returns `null` for unknown providers — this is correct behavior, not a gap.
  The gap is the fallback chain, not the null return.
- `stop-continuation-guard` is a placeholder — asserting it "doesn't stop" would pass.
  Assert the missing `tool.execute.before` hook instead.

## Task 4: Run lint and type check

Run from the **repo root**:

```bash
bunx biome check packages/opencode-plugin/src/__tests__/omo-replacement-matrix.test.ts
bunx tsc --noEmit -p packages/opencode-plugin/tsconfig.json
```

Fix any lint or type errors. The test file must be clean before the PR.

**Note on `await import()` calls:** The test uses dynamic `await import()` for hooks to avoid
TypeScript errors on missing exports. If TypeScript complains about the dynamic import return
type, use `as Record<string, unknown>` cast on the imported module.

## Task 5: Commit

```bash
jj describe -m "test(270): OMO replacement matrix contract tests — all 23 tests fail"
jj new
```

## Task 6: Create PR

```bash
gh pr create \
  --title "test(270): OMO replacement matrix contract tests (T1)" \
  --body "$(cat <<'EOF'
## Summary

Adds `packages/opencode-plugin/src/__tests__/omo-replacement-matrix.test.ts` with 23 contract
tests covering all 7 OMO capability areas. ALL tests fail on first run — this is intentional.
They define the acceptance gate for T6, T8, T10, T11, T16-T21.

## Test Results

\`\`\`
bun test packages/opencode-plugin/src/__tests__/omo-replacement-matrix.test.ts
\`\`\`

[PASTE FULL bun test OUTPUT HERE — must show all 23 tests failing with non-zero exit code]

## Coverage

| Area | Tests | Gap |
|------|-------|-----|
| 1. Skill system | 4 | Full gap — no skill keyword in src/ |
| 2. Spawn limits | 4 | Full gap — no depth/budget tracking |
| 3. Agents | 4 | Naming gap — OMO roles not mapped |
| 4. Tool guards | 3 | Write guard missing |
| 5. Context management | 3 | Configurable threshold missing |
| 6. Model fallback | 3 | Fallback chain missing |
| 7. Code quality | 2 | Stricter tsconfig options missing |

## Closes

Closes #270
EOF
)" \
  --base main \
  -R sjawhar/legion
```

## Testing Plan

### Setup

```bash
bun install
```

### Health Check

```bash
bun test packages/opencode-plugin/src/__tests__/integration.test.ts
# Must pass (existing tests unaffected)
```

### Verification Steps

1. **All contract tests fail**
   - Action: `bun test packages/opencode-plugin/src/__tests__/omo-replacement-matrix.test.ts`
   - Expected: Non-zero exit code, 23 failures, 0 passes
   - Tool: bun test

2. **Failure messages are meaningful**
   - Action: Read the test output
   - Expected: Each failure shows what was expected vs. received (not "cannot find module")
   - Tool: bun test output

3. **Test count ≥ 15**
   - Action: `grep -c "^\s*it(" packages/opencode-plugin/src/__tests__/omo-replacement-matrix.test.ts`
   - Expected: 23 (or ≥15)
   - Tool: grep

4. **All 7 areas covered with ≥2 tests each**
   - Action: Count tests per `describe` block in the output
   - Expected: Area 1: 4, Area 2: 4, Area 3: 4, Area 4: 3, Area 5: 3, Area 6: 3, Area 7: 2
   - Tool: bun test output

5. **Existing tests unaffected**
   - Action: `bun test packages/opencode-plugin/src/__tests__/integration.test.ts`
   - Expected: All existing tests pass (same as before this PR)
   - Tool: bun test

6. **Lint and type check clean**
   - Action: `bunx biome check packages/opencode-plugin/src/__tests__/omo-replacement-matrix.test.ts && bunx tsc --noEmit -p packages/opencode-plugin/tsconfig.json`
   - Expected: No errors
   - Tool: biome, tsc

### Skills to Invoke

No project-specific skills required for this task. Standard bun test runner.
