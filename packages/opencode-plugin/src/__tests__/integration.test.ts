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
      todo: async () => ({ data: [] }),
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
    it("creates 10 agents", () => {
      const agents = createAgents();
      expect(agents).toHaveLength(10);
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
        "conductor",
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
      try {
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
      } finally {
        fs.rmSync(tempRoot, { recursive: true, force: true });
      }
    });
  });

  describe("permissions", () => {
    it("sets global permission map", async () => {
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-permission-"));
      try {
        const ctx = createStubContext(tempRoot);
        const hooks = await OpenCodeLegion(ctx);
        const config: Record<string, unknown> = {};

        await hooks.config?.(config);

        const permission = (config as { permission?: Record<string, unknown> }).permission;
        expect(permission).toBeTruthy();
        expect((permission as { edit?: string }).edit).toBe("allow");
        expect((permission as { bash?: string }).bash).toBe("allow");
        expect((permission as { read?: string }).read).toBe("allow");
      } finally {
        fs.rmSync(tempRoot, { recursive: true, force: true });
      }
    });
  });

  describe("preemptive compaction", () => {
    it("triggers compaction at threshold and allows re-compaction", async () => {
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-compact-"));
      const summarizeCalls: Array<{ providerID: string; modelID: string; auto?: boolean }> = [];
      try {
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
      } finally {
        fs.rmSync(tempRoot, { recursive: true, force: true });
      }
    });

    it("skips compaction below threshold", async () => {
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-compact-low-"));
      let summarizeCalled = false;
      try {
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
      } finally {
        fs.rmSync(tempRoot, { recursive: true, force: true });
      }
    });

    it("skips compaction when modelID is missing", async () => {
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-compact-nomodel-"));
      let summarizeCalled = false;
      try {
        const ctx = createStubContext(tempRoot, {
          session: {
            messages: async () => ({
              data: [
                {
                  info: {
                    role: "assistant",
                    providerID: "openai",
                    tokens: {
                      input: 200_000,
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
      } finally {
        fs.rmSync(tempRoot, { recursive: true, force: true });
      }
    });

    it("swallows message fetch errors", async () => {
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-compact-error-"));
      let summarizeCalled = false;
      try {
        const ctx = createStubContext(tempRoot, {
          session: {
            messages: async () => {
              throw new Error("boom");
            },
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
      } finally {
        fs.rmSync(tempRoot, { recursive: true, force: true });
      }
    });
  });

  describe("conductor agent", () => {
    it("is included in agent list", () => {
      const agents = createAgents();
      expect(agents.map((a) => a.name)).toContain("conductor");
    });

    it("has delegation-only permissions via plugin config", async () => {
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-conductor-"));
      try {
        const ctx = createStubContext(tempRoot);
        const hooks = await OpenCodeLegion(ctx);
        const config: Record<string, unknown> = {};
        await hooks.config?.(config);

        const agentMap = config.agent as Record<string, { permission?: Record<string, string> }>;
        const conductor = agentMap?.conductor;
        expect(conductor).toBeTruthy();
        expect(conductor.permission?.edit).toBe("deny");
        expect(conductor.permission?.write).toBe("deny");
        expect(conductor.permission?.bash).toBe("deny");
        expect(conductor.permission?.task).toBe("allow");
        expect(conductor.permission?.read).toBe("allow");
        expect(conductor.permission?.glob).toBe("allow");
        expect(conductor.permission?.list).toBe("allow");
      } finally {
        fs.rmSync(tempRoot, { recursive: true, force: true });
      }
    });

    it("prompt enforces delegation-only constraints", () => {
      const agents = createAgents();
      const conductor = agents.find((a) => a.name === "conductor");
      expect(conductor).toBeTruthy();
      expect(conductor!.config.prompt).toContain("MUST NOT");
      expect(conductor!.config.prompt).not.toMatch(/claude|anthropic|gpt|openai|gemini|google/i);
    });

    it("prompt mentions background_task for delegation", () => {
      const agents = createAgents();
      const conductor = agents.find((a) => a.name === "conductor");
      expect(conductor).toBeTruthy();
      expect(conductor!.config.prompt).toContain("background_task");
    });

    it("prompt is within token budget (<3000 tokens)", () => {
      const agents = createAgents();
      const conductor = agents.find((a) => a.name === "conductor");
      expect(conductor!.config.prompt.length / 4).toBeLessThan(3000);
    });
  });

  describe("todo continuation enforcer", () => {
    it("injects continuation when session idles with incomplete todos", async () => {
      const sessionID = "session-continue";
      let capturedPrompt:
        | {
            agent?: string;
            model?: unknown;
            pathId: string;
            directory: string;
          }
        | undefined;
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-continue-"));
      try {
        const ctx = createStubContext(tempRoot, {
          session: {
            todo: async () => ({
              data: [
                { id: "1", content: "Task A", status: "in_progress", priority: "high" },
                { id: "2", content: "Task B", status: "pending", priority: "medium" },
              ],
            }),
            messages: async () => ({
              data: [
                {
                  info: {
                    role: "assistant",
                    agent: "orchestrator",
                    providerID: "anthropic",
                    modelID: "claude-sonnet-4-20250514",
                  },
                },
              ],
            }),
            promptAsync: async ({
              path: p,
              body,
              query,
            }: {
              path: { id: string };
              body: { agent?: string; model?: unknown; parts?: unknown[] };
              query: { directory: string };
            }) => {
              capturedPrompt = {
                agent: body.agent,
                model: body.model,
                pathId: p.id,
                directory: query.directory,
              };
              return {};
            },
          },
        });

        const { createTodoContinuationEnforcerHook } = await import(
          "../hooks/todo-continuation-enforcer"
        );
        const hook = createTodoContinuationEnforcerHook(ctx, {
          isContinuationStopped: () => false,
          isBackgroundSession: () => false,
          isRecovering: () => false,
          gracePeriodMs: 0,
        });

        await hook.event({ event: { type: "session.idle", properties: { sessionID } } });
        await new Promise((r) => setTimeout(r, 20));

        expect(capturedPrompt).toBeTruthy();
        expect(capturedPrompt!.agent).toBe("orchestrator");
        expect(capturedPrompt!.pathId).toBe(sessionID);
        expect(capturedPrompt!.directory).toBe(tempRoot);
        expect(capturedPrompt!.model).toEqual({
          providerID: "anthropic",
          modelID: "claude-sonnet-4-20250514",
        });
      } finally {
        fs.rmSync(tempRoot, { recursive: true, force: true });
      }
    });

    it("skips when all todos are complete", async () => {
      const sessionID = "session-all-done";
      let promptInjected = false;
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-all-done-"));
      try {
        const ctx = createStubContext(tempRoot, {
          session: {
            todo: async () => ({
              data: [{ id: "1", content: "Task A", status: "completed" }],
            }),
            messages: async () => ({ data: [] }),
            promptAsync: async () => {
              promptInjected = true;
              return {};
            },
          },
        });

        const { createTodoContinuationEnforcerHook } = await import(
          "../hooks/todo-continuation-enforcer"
        );
        const hook = createTodoContinuationEnforcerHook(ctx, {
          isContinuationStopped: () => false,
          isBackgroundSession: () => false,
          isRecovering: () => false,
          gracePeriodMs: 0,
        });

        await hook.event({ event: { type: "session.idle", properties: { sessionID } } });
        await new Promise((r) => setTimeout(r, 20));
        expect(promptInjected).toBe(false);
      } finally {
        fs.rmSync(tempRoot, { recursive: true, force: true });
      }
    });

    it("skips when todo fetch fails", async () => {
      const sessionID = "session-todo-error";
      let promptInjected = false;
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-todo-error-"));
      const originalWarn = console.warn;
      try {
        console.warn = () => {};
        const ctx = createStubContext(tempRoot, {
          session: {
            todo: async () => {
              throw new Error("todo failure");
            },
            messages: async () => ({
              data: [{ info: { role: "assistant", agent: "orchestrator" } }],
            }),
            promptAsync: async () => {
              promptInjected = true;
              return {};
            },
          },
        });

        const { createTodoContinuationEnforcerHook } = await import(
          "../hooks/todo-continuation-enforcer"
        );
        const hook = createTodoContinuationEnforcerHook(ctx, {
          isContinuationStopped: () => false,
          isBackgroundSession: () => false,
          isRecovering: () => false,
          gracePeriodMs: 0,
        });

        await hook.event({ event: { type: "session.idle", properties: { sessionID } } });
        expect(promptInjected).toBe(false);
      } finally {
        console.warn = originalWarn;
        fs.rmSync(tempRoot, { recursive: true, force: true });
      }
    });

    it("skips when continuation is stopped", async () => {
      const sessionID = "session-stopped";
      let promptInjected = false;
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-stopped-"));
      try {
        const ctx = createStubContext(tempRoot, {
          session: {
            todo: async () => ({
              data: [{ id: "1", content: "Task A", status: "pending" }],
            }),
            messages: async () => ({
              data: [{ info: { role: "assistant", agent: "orchestrator" } }],
            }),
            promptAsync: async () => {
              promptInjected = true;
              return {};
            },
          },
        });

        const { createTodoContinuationEnforcerHook } = await import(
          "../hooks/todo-continuation-enforcer"
        );
        const hook = createTodoContinuationEnforcerHook(ctx, {
          isContinuationStopped: () => true,
          isBackgroundSession: () => false,
          isRecovering: () => false,
          gracePeriodMs: 0,
        });

        await hook.event({ event: { type: "session.idle", properties: { sessionID } } });
        await new Promise((r) => setTimeout(r, 20));
        expect(promptInjected).toBe(false);
      } finally {
        fs.rmSync(tempRoot, { recursive: true, force: true });
      }
    });

    it("skips for non-continuation agents", async () => {
      const sessionID = "session-leaf";
      let promptInjected = false;
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-leaf-"));
      try {
        const ctx = createStubContext(tempRoot, {
          session: {
            todo: async () => ({
              data: [{ id: "1", content: "Task A", status: "pending" }],
            }),
            messages: async () => ({
              data: [{ info: { role: "assistant", agent: "explorer" } }],
            }),
            promptAsync: async () => {
              promptInjected = true;
              return {};
            },
          },
        });

        const { createTodoContinuationEnforcerHook } = await import(
          "../hooks/todo-continuation-enforcer"
        );
        const hook = createTodoContinuationEnforcerHook(ctx, {
          isContinuationStopped: () => false,
          isBackgroundSession: () => false,
          isRecovering: () => false,
          gracePeriodMs: 0,
        });

        await hook.event({ event: { type: "session.idle", properties: { sessionID } } });
        await new Promise((r) => setTimeout(r, 20));
        expect(promptInjected).toBe(false);
      } finally {
        fs.rmSync(tempRoot, { recursive: true, force: true });
      }
    });

    it("fails closed when agent cannot be resolved", async () => {
      const sessionID = "session-no-agent";
      let promptInjected = false;
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-no-agent-"));
      try {
        const ctx = createStubContext(tempRoot, {
          session: {
            todo: async () => ({
              data: [{ id: "1", content: "Task A", status: "pending" }],
            }),
            messages: async () => ({ data: [] }),
            promptAsync: async () => {
              promptInjected = true;
              return {};
            },
          },
        });

        const { createTodoContinuationEnforcerHook } = await import(
          "../hooks/todo-continuation-enforcer"
        );
        const hook = createTodoContinuationEnforcerHook(ctx, {
          isContinuationStopped: () => false,
          isBackgroundSession: () => false,
          isRecovering: () => false,
          gracePeriodMs: 0,
        });

        await hook.event({ event: { type: "session.idle", properties: { sessionID } } });
        await new Promise((r) => setTimeout(r, 20));
        expect(promptInjected).toBe(false);
      } finally {
        fs.rmSync(tempRoot, { recursive: true, force: true });
      }
    });

    it("skips for background sessions", async () => {
      const sessionID = "session-bg";
      let promptInjected = false;
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-bg-"));
      try {
        const ctx = createStubContext(tempRoot, {
          session: {
            todo: async () => ({
              data: [{ id: "1", content: "Task A", status: "pending" }],
            }),
            messages: async () => ({
              data: [{ info: { role: "assistant", agent: "orchestrator" } }],
            }),
            promptAsync: async () => {
              promptInjected = true;
              return {};
            },
          },
        });

        const { createTodoContinuationEnforcerHook } = await import(
          "../hooks/todo-continuation-enforcer"
        );
        const hook = createTodoContinuationEnforcerHook(ctx, {
          isContinuationStopped: () => false,
          isBackgroundSession: () => true,
          isRecovering: () => false,
          gracePeriodMs: 0,
        });

        await hook.event({ event: { type: "session.idle", properties: { sessionID } } });
        await new Promise((r) => setTimeout(r, 20));
        expect(promptInjected).toBe(false);
      } finally {
        fs.rmSync(tempRoot, { recursive: true, force: true });
      }
    });

    it("skips during recovery", async () => {
      const sessionID = "session-recovering";
      let promptInjected = false;
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-recovering-"));
      try {
        const ctx = createStubContext(tempRoot, {
          session: {
            todo: async () => ({
              data: [{ id: "1", content: "Task A", status: "pending" }],
            }),
            messages: async () => ({
              data: [{ info: { role: "assistant", agent: "orchestrator" } }],
            }),
            promptAsync: async () => {
              promptInjected = true;
              return {};
            },
          },
        });

        const { createTodoContinuationEnforcerHook } = await import(
          "../hooks/todo-continuation-enforcer"
        );
        const hook = createTodoContinuationEnforcerHook(ctx, {
          isContinuationStopped: () => false,
          isBackgroundSession: () => false,
          isRecovering: () => true,
          gracePeriodMs: 0,
        });

        await hook.event({ event: { type: "session.idle", properties: { sessionID } } });
        await new Promise((r) => setTimeout(r, 20));
        expect(promptInjected).toBe(false);
      } finally {
        fs.rmSync(tempRoot, { recursive: true, force: true });
      }
    });

    it("replaces timer on repeated idle (no duplicate injection)", async () => {
      const sessionID = "session-double-idle";
      let promptCount = 0;
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-double-"));
      try {
        const ctx = createStubContext(tempRoot, {
          session: {
            todo: async () => ({
              data: [{ id: "1", content: "Task A", status: "pending" }],
            }),
            messages: async () => ({
              data: [{ info: { role: "assistant", agent: "executor" } }],
            }),
            promptAsync: async () => {
              promptCount++;
              return {};
            },
          },
        });

        const { createTodoContinuationEnforcerHook } = await import(
          "../hooks/todo-continuation-enforcer"
        );
        const hook = createTodoContinuationEnforcerHook(ctx, {
          isContinuationStopped: () => false,
          isBackgroundSession: () => false,
          isRecovering: () => false,
          gracePeriodMs: 50,
        });

        await hook.event({ event: { type: "session.idle", properties: { sessionID } } });
        await hook.event({ event: { type: "session.idle", properties: { sessionID } } });
        await new Promise((r) => setTimeout(r, 150));

        expect(promptCount).toBe(1);
      } finally {
        fs.rmSync(tempRoot, { recursive: true, force: true });
      }
    });

    it("cancels pending continuation on user message", async () => {
      const sessionID = "session-cancel";
      let promptInjected = false;
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-cancel-"));
      try {
        const ctx = createStubContext(tempRoot, {
          session: {
            todo: async () => ({
              data: [{ id: "1", content: "Task A", status: "pending" }],
            }),
            messages: async () => ({
              data: [{ info: { role: "assistant", agent: "orchestrator" } }],
            }),
            promptAsync: async () => {
              promptInjected = true;
              return {};
            },
          },
        });

        const { createTodoContinuationEnforcerHook } = await import(
          "../hooks/todo-continuation-enforcer"
        );
        const hook = createTodoContinuationEnforcerHook(ctx, {
          isContinuationStopped: () => false,
          isBackgroundSession: () => false,
          isRecovering: () => false,
          gracePeriodMs: 100,
        });

        await hook.event({ event: { type: "session.idle", properties: { sessionID } } });
        await hook.chatMessage({ sessionID });
        await new Promise((r) => setTimeout(r, 200));
        expect(promptInjected).toBe(false);
      } finally {
        fs.rmSync(tempRoot, { recursive: true, force: true });
      }
    });

    it("cleans up on session.deleted", async () => {
      const sessionID = "session-cleanup";
      let promptInjected = false;
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-cleanup-"));
      try {
        const ctx = createStubContext(tempRoot, {
          session: {
            todo: async () => ({
              data: [{ id: "1", content: "Task A", status: "pending" }],
            }),
            messages: async () => ({
              data: [{ info: { role: "assistant", agent: "orchestrator" } }],
            }),
            promptAsync: async () => {
              promptInjected = true;
              return {};
            },
          },
        });

        const { createTodoContinuationEnforcerHook } = await import(
          "../hooks/todo-continuation-enforcer"
        );
        const hook = createTodoContinuationEnforcerHook(ctx, {
          isContinuationStopped: () => false,
          isBackgroundSession: () => false,
          isRecovering: () => false,
          gracePeriodMs: 100,
        });

        await hook.event({ event: { type: "session.idle", properties: { sessionID } } });
        await hook.event({
          event: { type: "session.deleted", properties: { info: { id: sessionID } } },
        });
        await new Promise((r) => setTimeout(r, 200));
        expect(promptInjected).toBe(false);
      } finally {
        fs.rmSync(tempRoot, { recursive: true, force: true });
      }
    });

    it("cleans up on session.deleted with sessionID format (C3)", async () => {
      const sessionID = "session-cleanup-c3";
      let promptInjected = false;
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-c3-"));
      try {
        const ctx = createStubContext(tempRoot, {
          session: {
            todo: async () => ({
              data: [{ id: "1", content: "Task A", status: "pending" }],
            }),
            messages: async () => ({
              data: [{ info: { role: "assistant", agent: "orchestrator" } }],
            }),
            promptAsync: async () => {
              promptInjected = true;
              return {};
            },
          },
        });

        const { createTodoContinuationEnforcerHook } = await import(
          "../hooks/todo-continuation-enforcer"
        );
        const hook = createTodoContinuationEnforcerHook(ctx, {
          isContinuationStopped: () => false,
          isBackgroundSession: () => false,
          isRecovering: () => false,
          gracePeriodMs: 100,
        });

        await hook.event({ event: { type: "session.idle", properties: { sessionID } } });
        await hook.event({
          event: { type: "session.deleted", properties: { sessionID } },
        });
        await new Promise((r) => setTimeout(r, 200));
        expect(promptInjected).toBe(false);
      } finally {
        fs.rmSync(tempRoot, { recursive: true, force: true });
      }
    });

    it("cancelPending prevents timer from firing (C1)", async () => {
      const sessionID = "session-cancel-pending";
      let promptInjected = false;
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-c1-"));
      try {
        const ctx = createStubContext(tempRoot, {
          session: {
            todo: async () => ({
              data: [{ id: "1", content: "Task A", status: "pending" }],
            }),
            messages: async () => ({
              data: [{ info: { role: "assistant", agent: "orchestrator" } }],
            }),
            promptAsync: async () => {
              promptInjected = true;
              return {};
            },
          },
        });

        const { createTodoContinuationEnforcerHook } = await import(
          "../hooks/todo-continuation-enforcer"
        );
        const hook = createTodoContinuationEnforcerHook(ctx, {
          isContinuationStopped: () => false,
          isBackgroundSession: () => false,
          isRecovering: () => false,
          gracePeriodMs: 100,
        });

        await hook.event({ event: { type: "session.idle", properties: { sessionID } } });
        hook.cancelPending(sessionID);
        await new Promise((r) => setTimeout(r, 200));
        expect(promptInjected).toBe(false);
      } finally {
        fs.rmSync(tempRoot, { recursive: true, force: true });
      }
    });

    it("re-checks isContinuationStopped when timer fires (C1)", async () => {
      const sessionID = "session-recheck-stop";
      let promptInjected = false;
      let stopped = false;
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-c1-recheck-"));
      try {
        const ctx = createStubContext(tempRoot, {
          session: {
            todo: async () => ({
              data: [{ id: "1", content: "Task A", status: "pending" }],
            }),
            messages: async () => ({
              data: [{ info: { role: "assistant", agent: "orchestrator" } }],
            }),
            promptAsync: async () => {
              promptInjected = true;
              return {};
            },
          },
        });

        const { createTodoContinuationEnforcerHook } = await import(
          "../hooks/todo-continuation-enforcer"
        );
        const hook = createTodoContinuationEnforcerHook(ctx, {
          isContinuationStopped: () => stopped,
          isBackgroundSession: () => false,
          isRecovering: () => false,
          gracePeriodMs: 50,
        });

        await hook.event({ event: { type: "session.idle", properties: { sessionID } } });
        stopped = true;
        await new Promise((r) => setTimeout(r, 150));
        expect(promptInjected).toBe(false);
      } finally {
        fs.rmSync(tempRoot, { recursive: true, force: true });
      }
    });
  });

  describe("stop-continuation + continuation integration (e2e)", () => {
    it("stop prevents continuation; user message clears stop; idle then continues", async () => {
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-e2e-stop-"));
      let promptCount = 0;
      try {
        writeJsonFile(path.join(tempRoot, ".opencode", "opencode-legion.json"), {
          continuationGracePeriodMs: 0,
        });
        const ctx = createStubContext(tempRoot, {
          session: {
            todo: async () => ({
              data: [{ id: "1", content: "Task A", status: "pending", priority: "high" }],
            }),
            messages: async () => ({
              data: [
                {
                  info: {
                    role: "assistant",
                    agent: "orchestrator",
                    providerID: "anthropic",
                    modelID: "test",
                  },
                },
              ],
            }),
            promptAsync: async () => {
              promptCount++;
              return {};
            },
          },
        });

        const hooks = await OpenCodeLegion(ctx);

        await hooks["tool.execute.before"]!(
          { tool: "slashcommand", sessionID: "ses1", callID: "1" },
          { args: { command: "stop-continuation" } }
        );

        await hooks.event!({
          event: { type: "session.idle", properties: { sessionID: "ses1" } },
        } as any);
        await new Promise((r) => setTimeout(r, 50));
        expect(promptCount).toBe(0);

        await hooks["chat.message"]!({ sessionID: "ses1" } as any, {} as any);

        await hooks.event!({
          event: { type: "session.idle", properties: { sessionID: "ses1" } },
        } as any);
        await new Promise((r) => setTimeout(r, 50));
        expect(promptCount).toBe(1);
      } finally {
        fs.rmSync(tempRoot, { recursive: true, force: true });
      }
    });
  });

  describe("compaction todo preserver", () => {
    it("restores todos after compaction when missing", async () => {
      const sessionID = "session-compact-missing";
      let capturedRestoreCall: { sessionID: string; todos: unknown[] } | undefined;
      let todoCalls = 0;
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-todo-preserve-"));
      try {
        const ctx = createStubContext(tempRoot, {
          session: {
            todo: async () => {
              todoCalls++;
              if (todoCalls === 1) {
                return {
                  data: [
                    { id: "1", content: "Task A", status: "pending", priority: "high" },
                    { id: "2", content: "Task B", status: "completed", priority: "low" },
                  ],
                };
              }
              return { data: [] };
            },
            todoUpdate: async ({
              path: p,
              body,
            }: {
              path: { id: string };
              body: { todos: unknown[] };
            }) => {
              capturedRestoreCall = { sessionID: p.id, todos: body.todos };
            },
          },
        });

        const { createCompactionTodoPreserverHook } = await import(
          "../hooks/compaction-todo-preserver"
        );
        const hook = createCompactionTodoPreserverHook(ctx);

        await hook.capture(sessionID);
        await hook.event({ event: { type: "session.compacted", properties: { sessionID } } });

        expect(capturedRestoreCall).toBeTruthy();
        expect(capturedRestoreCall!.sessionID).toBe(sessionID);
        expect(capturedRestoreCall!.todos).toHaveLength(2);
      } finally {
        fs.rmSync(tempRoot, { recursive: true, force: true });
      }
    });

    it("skips restore when todos still exist post-compaction", async () => {
      const sessionID = "session-compact-present";
      let todoUpdateCalled = false;
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-todo-skip-"));
      try {
        const ctx = createStubContext(tempRoot, {
          session: {
            todo: async () => ({
              data: [{ id: "1", content: "Still here", status: "pending", priority: "high" }],
            }),
            todoUpdate: async () => {
              todoUpdateCalled = true;
            },
          },
        });

        const { createCompactionTodoPreserverHook } = await import(
          "../hooks/compaction-todo-preserver"
        );
        const hook = createCompactionTodoPreserverHook(ctx);

        await hook.capture(sessionID);
        await hook.event({ event: { type: "session.compacted", properties: { sessionID } } });

        expect(todoUpdateCalled).toBe(false);
      } finally {
        fs.rmSync(tempRoot, { recursive: true, force: true });
      }
    });

    it("cleans up snapshot on session.deleted", async () => {
      const sessionID = "session-compact-cleanup";
      let todoUpdateCalled = false;
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-todo-cleanup-"));
      try {
        let todoCalls = 0;
        const ctx = createStubContext(tempRoot, {
          session: {
            todo: async () => {
              todoCalls++;
              return todoCalls === 1
                ? { data: [{ id: "1", content: "Task", status: "pending" }] }
                : { data: [] };
            },
            todoUpdate: async () => {
              todoUpdateCalled = true;
            },
          },
        });

        const { createCompactionTodoPreserverHook } = await import(
          "../hooks/compaction-todo-preserver"
        );
        const hook = createCompactionTodoPreserverHook(ctx);

        await hook.capture(sessionID);
        await hook.event({
          event: { type: "session.deleted", properties: { info: { id: sessionID } } },
        });
        await hook.event({ event: { type: "session.compacted", properties: { sessionID } } });

        expect(todoUpdateCalled).toBe(false);
      } finally {
        fs.rmSync(tempRoot, { recursive: true, force: true });
      }
    });

    it("skips restore when todoUpdate is unavailable", async () => {
      const sessionID = "session-compact-no-api";
      let todoCalls = 0;
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-todo-noapi-"));
      try {
        const ctx = createStubContext(tempRoot, {
          session: {
            todo: async () => {
              todoCalls++;
              return todoCalls === 1
                ? { data: [{ id: "1", content: "Task", status: "pending" }] }
                : { data: [] };
            },
          },
        });

        const { createCompactionTodoPreserverHook } = await import(
          "../hooks/compaction-todo-preserver"
        );
        const hook = createCompactionTodoPreserverHook(ctx);

        await hook.capture(sessionID);
        await hook.event({ event: { type: "session.compacted", properties: { sessionID } } });
      } finally {
        fs.rmSync(tempRoot, { recursive: true, force: true });
      }
    });

    it("merges back missing todos after partial loss (M5)", async () => {
      const sessionID = "session-m5-partial";
      let restoredTodos: unknown[] | undefined;
      let todoCalls = 0;
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-m5-"));
      try {
        const ctx = createStubContext(tempRoot, {
          session: {
            todo: async () => {
              todoCalls++;
              if (todoCalls === 1) {
                return {
                  data: [
                    { id: "1", content: "Task A", status: "pending", priority: "high" },
                    { id: "2", content: "Task B", status: "in_progress", priority: "medium" },
                    { id: "3", content: "Task C", status: "completed", priority: "low" },
                  ],
                };
              }
              // Post-compaction: only task 1 survived
              return {
                data: [{ id: "1", content: "Task A", status: "pending", priority: "high" }],
              };
            },
            todoUpdate: async ({ body }: { path: { id: string }; body: { todos: unknown[] } }) => {
              restoredTodos = body.todos;
            },
          },
        });

        const { createCompactionTodoPreserverHook } = await import(
          "../hooks/compaction-todo-preserver"
        );
        const hook = createCompactionTodoPreserverHook(ctx);

        await hook.capture(sessionID);
        await hook.event({ event: { type: "session.compacted", properties: { sessionID } } });

        expect(restoredTodos).toBeTruthy();
        expect(restoredTodos).toHaveLength(3);
        const ids = (restoredTodos as Array<{ id: string }>).map((t) => t.id);
        expect(ids).toContain("1");
        expect(ids).toContain("2");
        expect(ids).toContain("3");
      } finally {
        fs.rmSync(tempRoot, { recursive: true, force: true });
      }
    });

    it("preserves fresher status from subagent updates during compaction (M5)", async () => {
      const sessionID = "session-m5-fresh";
      let restoredTodos: Array<{ id: string; status: string }> | undefined;
      let todoCalls = 0;
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-m5-fresh-"));
      try {
        const ctx = createStubContext(tempRoot, {
          session: {
            todo: async () => {
              todoCalls++;
              if (todoCalls === 1) {
                return {
                  data: [
                    { id: "1", content: "Task A", status: "in_progress" },
                    { id: "2", content: "Task B", status: "pending" },
                  ],
                };
              }
              // Post-compaction: task 1 was updated by subagent to completed,
              // but task 2 was lost
              return {
                data: [{ id: "1", content: "Task A", status: "completed" }],
              };
            },
            todoUpdate: async ({
              body,
            }: {
              path: { id: string };
              body: { todos: Array<{ id: string; status: string }> };
            }) => {
              restoredTodos = body.todos;
            },
          },
        });

        const { createCompactionTodoPreserverHook } = await import(
          "../hooks/compaction-todo-preserver"
        );
        const hook = createCompactionTodoPreserverHook(ctx);

        await hook.capture(sessionID);
        await hook.event({ event: { type: "session.compacted", properties: { sessionID } } });

        expect(restoredTodos).toBeTruthy();
        expect(restoredTodos).toHaveLength(2);
        // Task 1 keeps the fresher "completed" status from current, not snapshot's "in_progress"
        const task1 = restoredTodos!.find((t) => t.id === "1");
        expect(task1?.status).toBe("completed");
        // Task 2 was restored from snapshot
        const task2 = restoredTodos!.find((t) => t.id === "2");
        expect(task2?.status).toBe("pending");
      } finally {
        fs.rmSync(tempRoot, { recursive: true, force: true });
      }
    });

    it("deletes stale snapshot when capture finds empty todos (M1)", async () => {
      const sessionID = "session-m1-stale";
      let todoUpdateCalled = false;
      let todoCalls = 0;
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-m1-"));
      try {
        const ctx = createStubContext(tempRoot, {
          session: {
            todo: async () => {
              todoCalls++;
              if (todoCalls === 1) {
                return { data: [{ id: "1", content: "Task", status: "pending" }] };
              }
              return { data: [] };
            },
            todoUpdate: async () => {
              todoUpdateCalled = true;
            },
          },
        });

        const { createCompactionTodoPreserverHook } = await import(
          "../hooks/compaction-todo-preserver"
        );
        const hook = createCompactionTodoPreserverHook(ctx);

        await hook.capture(sessionID);
        await hook.capture(sessionID);
        await hook.event({ event: { type: "session.compacted", properties: { sessionID } } });

        expect(todoUpdateCalled).toBe(false);
      } finally {
        fs.rmSync(tempRoot, { recursive: true, force: true });
      }
    });
  });

  describe("compaction context injector", () => {
    it("injects context template into compaction output via plugin", async () => {
      const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), "opencode-legion-compact-ctx-"));
      try {
        const ctx = createStubContext(tempRoot);
        const hooks = await OpenCodeLegion(ctx);
        const compactingHook = hooks["experimental.session.compacting"];
        expect(compactingHook).toBeTruthy();
        if (!compactingHook) throw new Error("Missing compacting hook");

        const output = { context: [] as string[] };
        await compactingHook({ sessionID: "session" }, output);

        expect(output.context.length).toBeGreaterThan(0);
        const template = output.context[0];
        expect(template).toContain("User Requests");
        expect(template).toContain("Remaining Tasks");
        expect(template).toContain("Active Working Context");
        expect(template).toContain("Explicit Constraints");
      } finally {
        fs.rmSync(tempRoot, { recursive: true, force: true });
      }
    });
  });

  describe("shared utils (extractTodos + resolveSessionID)", () => {
    it("extractTodos handles { data: [...] } wrapper", async () => {
      const { extractTodos } = await import("../hooks/utils");
      const result = extractTodos({ data: [{ id: "1", content: "A", status: "pending" }] });
      expect(result).toHaveLength(1);
      expect(result[0].id).toBe("1");
    });

    it("extractTodos handles raw array", async () => {
      const { extractTodos } = await import("../hooks/utils");
      const result = extractTodos([{ id: "2", content: "B", status: "completed" }]);
      expect(result).toHaveLength(1);
      expect(result[0].id).toBe("2");
    });

    it("extractTodos returns empty for non-array input", async () => {
      const { extractTodos } = await import("../hooks/utils");
      expect(extractTodos(null)).toEqual([]);
      expect(extractTodos(undefined)).toEqual([]);
      expect(extractTodos("string")).toEqual([]);
      expect(extractTodos({})).toEqual([]);
    });

    it("resolveSessionID handles { sessionID } shape", async () => {
      const { resolveSessionID } = await import("../hooks/utils");
      expect(resolveSessionID({ sessionID: "ses-1" })).toBe("ses-1");
    });

    it("resolveSessionID handles { info: { id } } shape", async () => {
      const { resolveSessionID } = await import("../hooks/utils");
      expect(resolveSessionID({ info: { id: "ses-2" } })).toBe("ses-2");
    });

    it("resolveSessionID prefers sessionID over info.id", async () => {
      const { resolveSessionID } = await import("../hooks/utils");
      expect(resolveSessionID({ sessionID: "ses-1", info: { id: "ses-2" } })).toBe("ses-1");
    });

    it("resolveSessionID returns undefined for empty props", async () => {
      const { resolveSessionID } = await import("../hooks/utils");
      expect(resolveSessionID(undefined)).toBeUndefined();
      expect(resolveSessionID({})).toBeUndefined();
    });
  });

  describe("plugin structure", () => {
    it("exports default plugin function", async () => {
      const mod = await import("../index");
      expect(typeof mod.default).toBe("function");
    });
  });
});
