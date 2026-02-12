import { describe, expect, it } from "bun:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { PluginInput, ToolContext } from "@opencode-ai/plugin";
import type { Project } from "@opencode-ai/sdk";
import { createAgents } from "../agents";
import { loadPluginConfig } from "../config";
import type { BackgroundTaskManager } from "../delegation";
import { createDelegationTools } from "../delegation";
import { resolveCategory } from "../delegation/category-router";
import { createPreemptiveCompactionHook } from "../hooks/preemptive-compaction";
import OpenCodeLegion from "../index";
import { getModelOverlay } from "../overlays";

function writeJsonFile(filePath: string, data: unknown): void {
  fs.mkdirSync(path.dirname(filePath), { recursive: true });
  fs.writeFileSync(filePath, `${JSON.stringify(data, null, 2)}\n`, "utf-8");
}

function createStubProject(worktree: string): Project {
  return {
    id: "project",
    worktree,
    time: { created: 0 },
  };
}

type ClientOverrides = {
  session?: Record<string, unknown>;
  tui?: Record<string, unknown>;
};

function createStubClient(overrides?: ClientOverrides): Record<string, unknown> {
  const client = {
    session: {
      create: async () => ({ data: { id: "session" } }),
      promptAsync: async () => ({}),
      messages: async () => ({ data: [] }),
      summarize: async () => ({}),
      abort: async () => ({}),
      list: async () => ({ data: [] }),
      get: async () => ({ data: {} }),
    },
    tui: {
      showToast: async () => ({}),
    },
  };

  if (!overrides) return client;

  return {
    ...client,
    ...overrides,
    session: {
      ...client.session,
      ...(overrides.session ?? {}),
    },
    tui: {
      ...client.tui,
      ...(overrides.tui ?? {}),
    },
  };
}

function createStubContext(directory: string, overrides?: ClientOverrides): PluginInput {
  const shell = (() =>
    Promise.resolve({
      stdout: Buffer.alloc(0),
      stderr: Buffer.alloc(0),
      exitCode: 0,
      text: () => "",
      json: () => ({}),
      arrayBuffer: () => new ArrayBuffer(0),
      bytes: () => new Uint8Array(),
      blob: () => new Blob(),
    })) as unknown as PluginInput["$"];

  return {
    client: createStubClient(overrides) as unknown as PluginInput["client"],
    project: createStubProject(directory),
    directory,
    worktree: directory,
    serverUrl: new URL("http://localhost"),
    $: shell,
  };
}

function createToolContext(directory: string, agent = "orchestrator"): ToolContext {
  return {
    sessionID: "session",
    messageID: "message",
    agent,
    directory,
    worktree: directory,
    abort: new AbortController().signal,
    metadata: () => {},
    ask: async () => {},
  };
}

describe("opencode-legion plugin", () => {
  describe("agents", () => {
    it("creates 8 agents", () => {
      const agents = createAgents();
      expect(agents).toHaveLength(8);
    });

    it("all agents have name, description, and config", () => {
      const agents = createAgents();
      for (const agent of agents) {
        expect(agent.name).toBeTruthy();
        expect(agent.description).toBeTruthy();
        expect(agent.config.model).toBeTruthy();
        expect(typeof agent.config.temperature).toBe("number");
        expect(agent.config.prompt).toBeTruthy();
      }
    });

    it("agent names are unique", () => {
      const agents = createAgents();
      const names = agents.map((a) => a.name);
      expect(new Set(names).size).toBe(names.length);
    });

    it("includes all expected agents", () => {
      const agents = createAgents();
      const names = agents.map((a) => a.name);
      const expected = [
        "orchestrator",
        "executor",
        "oracle",
        "explorer",
        "librarian",
        "metis",
        "momus",
        "multimodal",
      ];
      for (const name of expected) {
        expect(names).toContain(name);
      }
    });

    it("respects config overrides", () => {
      const agents = createAgents({
        agents: { orchestrator: { model: "custom/model" } },
      });
      const orchestrator = agents.find((a) => a.name === "orchestrator");
      expect(orchestrator?.config.model).toBe("custom/model");
    });

    it("respects temperature overrides", () => {
      const agents = createAgents({
        agents: { executor: { temperature: 0.8 } },
      });
      const executor = agents.find((a) => a.name === "executor");
      expect(executor?.config.temperature).toBe(0.8);
    });

    it("config override only affects targeted agent", () => {
      const agents = createAgents({
        agents: { orchestrator: { model: "custom/model" } },
      });
      const executor = agents.find((a) => a.name === "executor");
      expect(executor?.config.model).not.toBe("custom/model");
      expect(executor?.config.model).toBeTruthy();
    });

    it("prompts are model-neutral (no model-specific text)", () => {
      const agents = createAgents();
      const modelTerms = /claude|anthropic|gpt|openai|gemini|google/i;
      for (const agent of agents) {
        expect(agent.config.prompt).not.toMatch(modelTerms);
      }
    });

    it("prompts are within token budget (<3000 tokens)", () => {
      const agents = createAgents();
      for (const agent of agents) {
        const estimatedTokens = agent.config.prompt.length / 4;
        expect(estimatedTokens).toBeLessThan(3000);
      }
    });
  });

  describe("category routing", () => {
    it("resolves all 8 categories", () => {
      const categories = [
        "visual-engineering",
        "ultrabrain",
        "deep",
        "artistry",
        "quick",
        "unspecified-low",
        "unspecified-high",
        "writing",
      ];
      for (const cat of categories) {
        const config = resolveCategory(cat);
        expect(config).toBeTruthy();
        expect(config.model).toBeTruthy();
        expect(typeof config.temperature).toBe("number");
      }
    });

    it("each category has a description", () => {
      const categories = [
        "visual-engineering",
        "ultrabrain",
        "deep",
        "artistry",
        "quick",
        "unspecified-low",
        "unspecified-high",
        "writing",
      ];
      for (const cat of categories) {
        const config = resolveCategory(cat);
        expect(config.description).toBeTruthy();
      }
    });

    it("unknown category falls back to unspecified-low", () => {
      const config = resolveCategory("nonexistent");
      const fallback = resolveCategory("unspecified-low");
      expect(config.model).toBe(fallback.model);
      expect(config.temperature).toBe(fallback.temperature);
      expect(config.description).toBe(fallback.description);
    });

    it("user config overrides defaults", () => {
      const config = resolveCategory("ultrabrain", {
        ultrabrain: {
          defaultModel: "custom/model",
          description: "custom",
        },
      });
      expect(config.model).toBe("custom/model");
      expect(config.description).toBe("custom");
    });

    it("user config for one category does not affect others", () => {
      const overridden = resolveCategory("ultrabrain", {
        ultrabrain: {
          defaultModel: "custom/model",
          description: "custom",
        },
      });
      const untouched = resolveCategory("quick", {
        ultrabrain: {
          defaultModel: "custom/model",
          description: "custom",
        },
      });
      expect(overridden.model).toBe("custom/model");
      expect(untouched.model).not.toBe("custom/model");
    });

    it("model override takes precedence over category default", () => {
      const config = resolveCategory("ultrabrain", undefined, "override/model");
      expect(config.model).toBe("override/model");
      expect(config.defaultModel).toBe("anthropic/claude-opus-4-6");
    });

    it("model override takes precedence over user config default", () => {
      const config = resolveCategory(
        "ultrabrain",
        { ultrabrain: { defaultModel: "user/model" } },
        "override/model"
      );
      expect(config.model).toBe("override/model");
      expect(config.defaultModel).toBe("user/model");
    });
  });

  describe("model overlays", () => {
    it("returns overlay for anthropic provider", () => {
      const overlay = getModelOverlay("anthropic", "claude-opus-4-6");
      expect(overlay).toBeTruthy();
      expect(overlay?.provider).toBe("anthropic");
      expect(overlay?.systemContent).toBeTruthy();
    });

    it("returns overlay for openai provider", () => {
      const overlay = getModelOverlay("openai", "gpt-5.2-codex");
      expect(overlay).toBeTruthy();
      expect(overlay?.provider).toBe("openai");
      expect(overlay?.systemContent).toBeTruthy();
    });

    it("returns overlay for google provider", () => {
      const overlay = getModelOverlay("google", "gemini-3-pro");
      expect(overlay).toBeTruthy();
      expect(overlay?.provider).toBe("google");
      expect(overlay?.systemContent).toBeTruthy();
    });

    it("returns null for unknown provider", () => {
      const overlay = getModelOverlay("unknown", "model");
      expect(overlay).toBeNull();
    });

    it("overlays are within token budget (<500 tokens)", () => {
      const providers = [
        { id: "anthropic", model: "claude-opus-4-6" },
        { id: "openai", model: "gpt-5.2-codex" },
        { id: "google", model: "gemini-3-pro" },
      ];
      for (const p of providers) {
        const overlay = getModelOverlay(p.id, p.model);
        if (overlay) {
          const tokens = overlay.systemContent.length / 4;
          expect(tokens).toBeLessThan(500);
        }
      }
    });

    it("overlays differ by provider (dispatch-time, not init-time)", () => {
      const claude = getModelOverlay("anthropic", "claude-opus-4-6");
      const gpt = getModelOverlay("openai", "gpt-5.2-codex");
      const gemini = getModelOverlay("google", "gemini-3-pro");

      expect(claude?.systemContent).not.toBe(gpt?.systemContent);
      expect(claude?.systemContent).not.toBe(gemini?.systemContent);
      expect(gpt?.systemContent).not.toBe(gemini?.systemContent);
    });

    it("overlays contain model-guidance tags", () => {
      const providers = [
        { id: "anthropic", model: "claude-opus-4-6" },
        { id: "openai", model: "gpt-5.2-codex" },
        { id: "google", model: "gemini-3-pro" },
      ];
      for (const p of providers) {
        const overlay = getModelOverlay(p.id, p.model);
        expect(overlay?.systemContent).toContain("<model-guidance>");
        expect(overlay?.systemContent).toContain("</model-guidance>");
      }
    });
  });

  describe("runtime config", () => {
    it("merges repo config over user config", async () => {
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-config-"));
      const repoDir = path.join(tempRoot, "repo");

      try {
        writeJsonFile(path.join(tempRoot, ".config", "opencode", "opencode-legion.json"), {
          agents: { executor: { model: "user/model", temperature: 0.2 } },
          categories: {
            deep: { defaultModel: "user/deep", description: "user" },
          },
        });

        writeJsonFile(path.join(repoDir, ".opencode", "opencode-legion.json"), {
          agents: { executor: { model: "repo/model" } },
          categories: {
            deep: { defaultModel: "repo/deep" },
          },
        });

        const config = await loadPluginConfig(repoDir, { homeDir: tempRoot });
        expect(config.agents?.executor?.model).toBe("repo/model");
        expect(config.agents?.executor?.temperature).toBe(0.2);
        expect(config.categories?.deep?.defaultModel).toBe("repo/deep");
        expect(config.categories?.deep?.description).toBe("user");
      } finally {
        fs.rmSync(tempRoot, { recursive: true, force: true });
      }
    });
  });

  describe("delegation model routing", () => {
    it("passes category model to background task launch", async () => {
      let capturedModel: string | undefined;
      const manager = {
        launch: (opts: { agent: string; model?: string; description: string }) => {
          capturedModel = opts.model;
          return {
            id: "bg_test",
            status: "pending",
            agent: opts.agent,
            model: opts.model ?? "",
            description: opts.description,
            createdAt: Date.now(),
          };
        },
        getTaskOutput: async () => "",
        cancel: () => false,
        cancelAll: () => 0,
      } as unknown as BackgroundTaskManager;

      const tools = createDelegationTools(manager, {
        categories: {
          deep: { defaultModel: "openai/test-model", description: "custom" },
        },
      });

      await tools.background_task.execute(
        {
          prompt: "hello",
          description: "test",
          category: "deep",
        },
        createToolContext("/tmp")
      );

      expect(capturedModel).toBe("openai/test-model");
    });

    it("uses agent model when category is omitted", async () => {
      let capturedModel: string | undefined;
      const manager = {
        launch: (opts: { agent: string; model?: string; description: string }) => {
          capturedModel = opts.model;
          return {
            id: "bg_test",
            status: "pending",
            agent: opts.agent,
            model: opts.model ?? "",
            description: opts.description,
            createdAt: Date.now(),
          };
        },
        getTaskOutput: async () => "",
        cancel: () => false,
        cancelAll: () => 0,
      } as unknown as BackgroundTaskManager;

      const tools = createDelegationTools(manager, {
        agents: {
          explorer: { model: "anthropic/custom-explorer" },
        },
      });

      await tools.background_task.execute(
        {
          prompt: "hello",
          description: "test",
          subagent_type: "explorer",
        },
        createToolContext("/tmp")
      );

      expect(capturedModel).toBe("anthropic/custom-explorer");
    });
  });

  describe("overlay injection", () => {
    it("injects overlay once via system hook", async () => {
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-overlay-"));
      const ctx = createStubContext(tempRoot);
      const hooks = await OpenCodeLegion(ctx);
      const systemHook = hooks["experimental.chat.system.transform"];
      expect(systemHook).toBeTruthy();
      if (!systemHook) throw new Error("Missing system transform hook");

      const output = { system: [] as string[] };
      const systemInput = {
        model: { providerID: "openai", id: "gpt-5.2-codex" },
      } as Parameters<typeof systemHook>[0];
      await systemHook(systemInput, output);

      expect(output.system).toHaveLength(1);

      await systemHook(systemInput, output);

      expect(output.system).toHaveLength(1);
      fs.rmSync(tempRoot, { recursive: true, force: true });
    });
  });

  describe("permissions", () => {
    it("sets global permission map", async () => {
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-permission-"));
      const ctx = createStubContext(tempRoot);
      const hooks = await OpenCodeLegion(ctx);
      const config: Record<string, unknown> = {};

      await hooks.config?.(config);

      const permission = (config as { permission?: Record<string, unknown> }).permission;
      expect(permission).toBeTruthy();
      expect((permission as { edit?: string }).edit).toBe("allow");
      expect((permission as { bash?: string }).bash).toBe("allow");
      expect((permission as { read?: string }).read).toBe("allow");

      fs.rmSync(tempRoot, { recursive: true, force: true });
    });
  });

  describe("preemptive compaction", () => {
    it("triggers compaction at threshold and allows re-compaction", async () => {
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-compact-"));
      const summarizeCalls: Array<{ providerID: string; modelID: string; auto?: boolean }> = [];

      const ctx = createStubContext(tempRoot, {
        session: {
          messages: async () => ({
            data: [
              {
                info: {
                  role: "assistant",
                  providerID: "openai",
                  modelID: "gpt-5.2-codex",
                  tokens: {
                    input: 100_000,
                    output: 0,
                    reasoning: 0,
                    cache: { read: 0, write: 0 },
                  },
                },
              },
            ],
          }),
          summarize: async ({
            body,
          }: {
            body: { providerID: string; modelID: string; auto?: boolean };
          }) => {
            summarizeCalls.push(body);
            return {};
          },
        },
      });

      const hook = createPreemptiveCompactionHook(ctx);
      const toolAfter = hook["tool.execute.after"];
      await toolAfter?.(
        { tool: "grep", sessionID: "session", callID: "1" },
        { title: "", output: "", metadata: {} }
      );
      await toolAfter?.(
        { tool: "grep", sessionID: "session", callID: "2" },
        { title: "", output: "", metadata: {} }
      );

      expect(summarizeCalls).toHaveLength(2);
      expect(summarizeCalls[0]).toMatchObject({
        providerID: "openai",
        modelID: "gpt-5.2-codex",
        auto: true,
      });

      fs.rmSync(tempRoot, { recursive: true, force: true });
    });

    it("skips compaction below threshold", async () => {
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-compact-low-"));
      let summarizeCalled = false;
      const ctx = createStubContext(tempRoot, {
        session: {
          messages: async () => ({
            data: [
              {
                info: {
                  role: "assistant",
                  providerID: "openai",
                  modelID: "gpt-5.2-codex",
                  tokens: {
                    input: 1_000,
                    output: 0,
                    reasoning: 0,
                    cache: { read: 0, write: 0 },
                  },
                },
              },
            ],
          }),
          summarize: async () => {
            summarizeCalled = true;
            return {};
          },
        },
      });

      const hook = createPreemptiveCompactionHook(ctx);
      await hook["tool.execute.after"]?.(
        { tool: "grep", sessionID: "session", callID: "1" },
        { title: "", output: "", metadata: {} }
      );

      expect(summarizeCalled).toBe(false);
      fs.rmSync(tempRoot, { recursive: true, force: true });
    });
  });

  describe("plugin structure", () => {
    it("exports default plugin function", async () => {
      const mod = await import("../index");
      expect(typeof mod.default).toBe("function");
    });
  });
});
