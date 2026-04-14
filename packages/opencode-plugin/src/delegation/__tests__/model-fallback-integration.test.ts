/**
 * Model Fallback Chain — Integration Tests
 *
 * Tests the fallback chain wired through BackgroundTaskManager and LaunchOptions.
 * Verifies that when a model fails during startPrompt, fallback models are tried.
 */

import { afterEach, beforeEach, describe, expect, it } from "bun:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { PluginInput } from "@opencode-ai/plugin";
import { BackgroundTaskManager } from "../background-manager";

let workspace: string;

/**
 * Poll task status until it leaves "running"/"pending" or max wait is reached.
 * Avoids fragile setTimeout-based waits for fire-and-forget startPrompt.
 */
async function waitForTaskSettled(
  task: { status: string },
  maxWaitMs = 2000,
  intervalMs = 10
): Promise<void> {
  const deadline = Date.now() + maxWaitMs;
  while ((task.status === "running" || task.status === "pending") && Date.now() < deadline) {
    await new Promise((resolve) => setTimeout(resolve, intervalMs));
  }
}

function createManager(
  overrides: Partial<{
    create: () => Promise<{ data?: { id?: string } }>;
    promptAsync: (args: unknown) => Promise<void>;
    messages: () => Promise<{ data?: unknown[] }>;
    abort: () => Promise<void>;
  }> = {}
): {
  manager: BackgroundTaskManager;
  session: {
    create: () => Promise<{ data?: { id?: string } }>;
    promptAsync: (args: unknown) => Promise<void>;
    messages: () => Promise<{ data?: unknown[] }>;
    abort: () => Promise<void>;
  };
} {
  const session = {
    create: async () => ({ data: { id: "session-1" } }),
    promptAsync: async () => {},
    messages: async () => ({ data: [] }),
    abort: async () => {},
    ...overrides,
  };
  const client = { session };
  const manager = new BackgroundTaskManager({
    client,
    directory: workspace,
  } as unknown as PluginInput);

  return { manager, session };
}

beforeEach(() => {
  workspace = fs.mkdtempSync(path.join(os.tmpdir(), "fallback-integ-test-"));
});

afterEach(() => {
  fs.rmSync(workspace, { recursive: true, force: true });
});

describe("BackgroundTaskManager with fallback chains", () => {
  it("LaunchOptions accepts fallbackModels array", async () => {
    const { manager } = createManager();

    const task = await manager.launch({
      agent: "executor",
      prompt: "test prompt",
      description: "test",
      model: "model-a",
      fallbackModels: ["model-b", "model-c"],
    });

    expect(task.model).toBe("model-a");
    // Task should be created with primary model initially
    expect(task.status).not.toBe("failed");
  });

  it("tries fallback model when primary model fails in startPrompt", async () => {
    const promptCalls: Array<{ providerID: string; modelID: string }> = [];
    let callCount = 0;

    const { manager } = createManager({
      promptAsync: async (args: unknown) => {
        const body = (args as { body: { model: { providerID: string; modelID: string } } }).body;
        promptCalls.push(body.model);
        callCount++;
        if (callCount === 1) {
          throw new Error("model-a rate limited");
        }
        // Second call succeeds
      },
    });

    const task = await manager.launch({
      agent: "executor",
      prompt: "test prompt",
      description: "test",
      model: "provider/model-a",
      fallbackModels: ["provider/model-b"],
    });

    await waitForTaskSettled(task);

    expect(promptCalls.length).toBe(2);
    expect(promptCalls[0]?.modelID).toBe("model-a");
    expect(promptCalls[1]?.modelID).toBe("model-b");
  });

  it("updates task model to the successful fallback model", async () => {
    let callCount = 0;

    const { manager } = createManager({
      promptAsync: async () => {
        callCount++;
        if (callCount === 1) {
          throw new Error("primary failed");
        }
      },
    });

    const task = await manager.launch({
      agent: "executor",
      prompt: "test prompt",
      description: "test",
      model: "anthropic/claude-opus-4-6",
      fallbackModels: ["openai/gpt-5.2-codex"],
    });

    await waitForTaskSettled(task);

    // Task model should reflect the model that actually succeeded
    expect(task.model).toBe("openai/gpt-5.2-codex");
  });

  it("fails task when all models in fallback chain fail", async () => {
    const { manager } = createManager({
      promptAsync: async () => {
        throw new Error("model unavailable");
      },
    });

    const task = await manager.launch({
      agent: "executor",
      prompt: "test prompt",
      description: "test",
      model: "model-a",
      fallbackModels: ["model-b"],
    });

    await waitForTaskSettled(task);

    expect(task.status).toBe("failed");
    expect(task.error).toContain("model-a");
    expect(task.error).toContain("model-b");
  });

  it("no fallbackModels means single attempt behavior unchanged", async () => {
    let callCount = 0;

    const { manager } = createManager({
      promptAsync: async () => {
        callCount++;
        throw new Error("model failed");
      },
    });

    const task = await manager.launch({
      agent: "executor",
      prompt: "test prompt",
      description: "test",
      model: "provider/model-a",
      // No fallbackModels
    });

    await waitForTaskSettled(task);

    expect(callCount).toBe(1);
    expect(task.status).toBe("failed");
  });
});
