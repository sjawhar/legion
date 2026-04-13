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
 * When implementing a capability, the corresponding test(s) will start passing —
 * that's the acceptance signal.
 *
 * Clean-room implementation — no OMO source code copied.
 */

import { describe, expect, it } from "bun:test";
import tsconfig from "../../tsconfig.json";
import { createAgents } from "../agents";
import * as delegationIndex from "../delegation";
import { BackgroundTaskManager } from "../delegation";
import * as pluginIndex from "../index";
import * as overlaysIndex from "../overlays";

// Helper: cast module/object to a string-keyed record for dynamic property access.
function asRecord(value: unknown): Record<string, unknown> {
  return value as Record<string, unknown>;
}

// ─── Area 1: Skill System ────────────────────────────────────────────────────
// OMO gates MCP tools behind skills — tools only load when the skill is active.
// opencode-legion has no skill system yet (confirmed: no "skill" keyword in src/).
// These tests assert the missing exports that the skill system must provide.

describe("Area 1: Skill system", () => {
  it("exports discoverSkills from plugin index", () => {
    expect(asRecord(pluginIndex).discoverSkills).toBeDefined();
  });

  it("exports parseSkill from plugin index", () => {
    expect(asRecord(pluginIndex).parseSkill).toBeDefined();
  });

  // Key architectural requirement: tools keyed by (sessionID, skillName, serverName)
  it("exports createSkillMcpManager from plugin index", () => {
    expect(asRecord(pluginIndex).createSkillMcpManager).toBeDefined();
  });

  it("exports injectSkill from plugin index", () => {
    expect(asRecord(pluginIndex).injectSkill).toBeDefined();
  });
});

// ─── Area 2: Spawn Limits ────────────────────────────────────────────────────
// OMO tracks spawn depth and descendant budget to prevent runaway delegation.
// BackgroundTaskManager has private depth tracking (resolveSpawnContext,
// validateAndReserveSpawn) but no PUBLIC API for querying depth/descendants.
// LaunchOptions has no maxDepth field — limits are only in SpawnLimitsConfig.
// These tests assert the missing PUBLIC interface that consumers need.

describe("Area 2: Spawn limits", () => {
  // resolveSpawnContext is private — needs a public getDepth(sessionId)
  it("BackgroundTaskManager exposes a public getDepth method", () => {
    const proto = asRecord(BackgroundTaskManager.prototype);
    expect(proto.getDepth).toBeDefined();
  });

  // rootDescendantCounts is private — needs a public getDescendantCount(sessionId)
  it("BackgroundTaskManager exposes a public getDescendantCount method", () => {
    const proto = asRecord(BackgroundTaskManager.prototype);
    expect(proto.getDescendantCount).toBeDefined();
  });

  // SpawnLimitsConfig has maxDepth as a per-instance config value, but no exported constant
  it("exports MAX_SPAWN_DEPTH constant from delegation index", () => {
    expect(asRecord(delegationIndex).MAX_SPAWN_DEPTH).toBeDefined();
  });

  // validateAndReserveSpawn is private — needs a public enforceSpawnLimits API
  it("BackgroundTaskManager exposes a public enforceSpawnLimits method", () => {
    const proto = asRecord(BackgroundTaskManager.prototype);
    expect(proto.enforceSpawnLimits).toBeDefined();
  });
});

// ─── Area 3: Agents ──────────────────────────────────────────────────────────
// OMO defines 4 named roles: Atlas, Hephaestus, Prometheus, Athena.
// opencode-legion has 10 agents with different names (orchestrator, executor, etc.).
// These todos assert the OMO role names SHOULD be present (as aliases or mappings).

describe("Area 3: Agents (OMO role mapping)", () => {
  it("Atlas agent role alias is present in createAgents() output", () => {
    const agentNames = createAgents().map((a) => a.name);
    expect(agentNames).toContain("atlas");
  });

  it("Hephaestus agent role alias is present in createAgents() output", () => {
    const agentNames = createAgents().map((a) => a.name);
    expect(agentNames).toContain("hephaestus");
  });

  it("Prometheus agent role alias is present in createAgents() output", () => {
    const agentNames = createAgents().map((a) => a.name);
    expect(agentNames).toContain("prometheus");
  });

  it("Athena agent role alias is present in createAgents() output", () => {
    const agentNames = createAgents().map((a) => a.name);
    expect(agentNames).toContain("athena");
  });
});

// ─── Area 4: Tool Guards ─────────────────────────────────────────────────────
// OMO has a write-existing-file guard that blocks writes to files not yet read.
// opencode-legion has stop-continuation-guard (placeholder) and subagent-question-blocker,
// but NO write-existing-file guard.

describe("Area 4: Tool guards", () => {
  it("exports createWriteExistingFileGuard from plugin index", () => {
    expect(asRecord(pluginIndex).createWriteExistingFileGuard).toBeDefined();
  });

  // The guard returns { event, "chat.message", stop, isStopped, clear } — no tool.execute.before
  it("stop-continuation-guard wires a tool.execute.before hook", async () => {
    const { createStopContinuationGuardHook } = await import("../hooks/stop-continuation-guard");
    const guard = createStopContinuationGuardHook();
    expect(asRecord(guard)["tool.execute.before"]).toBeDefined();
  });

  it("exports createReadFileRegistry for per-session read-file tracking", () => {
    expect(asRecord(pluginIndex).createReadFileRegistry).toBeDefined();
  });
});

// ─── Area 5: Context Management ──────────────────────────────────────────────
// OMO has a configurable context window monitor. opencode-legion has preemptive-compaction
// with a HARDCODED 0.78 threshold. The missing piece: configurable threshold.

describe("Area 5: Context management", () => {
  // Currently takes 1 param (ctx). Needs to accept (ctx, options) with threshold.
  it("createPreemptiveCompactionHook accepts a threshold option", async () => {
    const { createPreemptiveCompactionHook } = await import("../hooks/preemptive-compaction");
    expect(createPreemptiveCompactionHook.length).toBeGreaterThanOrEqual(2);
  });

  it("compaction threshold is configurable via plugin config", () => {
    expect(asRecord(pluginIndex).CONFIGURABLE_COMPACTION_THRESHOLD).toBeDefined();
  });

  // Template currently has sections for User Requests, Final Goal, etc. but NOT Tool Call History
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
  it("exports createModelFallbackChain from overlays", () => {
    expect(asRecord(overlaysIndex).createModelFallbackChain).toBeDefined();
  });

  // Currently: getModelOverlay(providerID, modelID) -> 2 params. Needs fallbacks? param.
  it("getModelOverlay supports fallback chain configuration", () => {
    expect(overlaysIndex.getModelOverlay.length).toBeGreaterThanOrEqual(3);
  });

  it("exports createRetryWithFallback from delegation index", () => {
    expect(asRecord(delegationIndex).createRetryWithFallback).toBeDefined();
  });
});

// ─── Area 7: Code Quality ────────────────────────────────────────────────────
// OMO enforces stricter TypeScript options. opencode-legion has strict: true and
// Biome, but is missing stricter options that OMO requires.

describe("Area 7: Code quality (stricter gates)", () => {
  it("tsconfig enables noUncheckedIndexedAccess", () => {
    expect(tsconfig.compilerOptions).toHaveProperty("noUncheckedIndexedAccess", true);
  });

  it("tsconfig enables exactOptionalPropertyTypes", () => {
    expect(tsconfig.compilerOptions).toHaveProperty("exactOptionalPropertyTypes", true);
  });
});
