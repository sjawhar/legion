/**
 * Model Fallback Chain Tests (T6 — OMO Replacement)
 *
 * Tests the model fallback chain system: when a model request fails,
 * the system tries alternative models in sequence before giving up.
 *
 * TDD: All tests written before implementation.
 */

import { describe, expect, it } from "bun:test";

// ─── executeWithModelFallback ────────────────────────────────────────────────

describe("executeWithModelFallback", () => {
  it("succeeds on first model when it works", async () => {
    const { executeWithModelFallback } = await import("../../delegation");
    const { createModelFallbackChain } = await import("../../overlays");

    const chain = createModelFallbackChain(["model-a", "model-b", "model-c"]);
    const attempts: string[] = [];

    const result = await executeWithModelFallback(chain.models, async (model) => {
      attempts.push(model);
      return `result-from-${model}`;
    });

    expect(result.model).toBe("model-a");
    expect(result.result).toBe("result-from-model-a");
    expect(result.attempts).toBe(1);
    expect(attempts).toEqual(["model-a"]);
  });

  it("falls back to second model when first fails", async () => {
    const { executeWithModelFallback } = await import("../../delegation");
    const { createModelFallbackChain } = await import("../../overlays");

    const chain = createModelFallbackChain(["model-a", "model-b", "model-c"]);
    const attempts: string[] = [];

    const result = await executeWithModelFallback(chain.models, async (model) => {
      attempts.push(model);
      if (model === "model-a") throw new Error("model-a unavailable");
      return `result-from-${model}`;
    });

    expect(result.model).toBe("model-b");
    expect(result.result).toBe("result-from-model-b");
    expect(result.attempts).toBe(2);
    expect(attempts).toEqual(["model-a", "model-b"]);
  });

  it("falls back through entire chain [A, B, C]: A fails, B fails, C succeeds", async () => {
    const { executeWithModelFallback } = await import("../../delegation");
    const { createModelFallbackChain } = await import("../../overlays");

    const chain = createModelFallbackChain(["model-a", "model-b", "model-c"]);
    const attempts: string[] = [];

    const result = await executeWithModelFallback(chain.models, async (model) => {
      attempts.push(model);
      if (model === "model-a") throw new Error("model-a unavailable");
      if (model === "model-b") throw new Error("model-b rate limited");
      return `result-from-${model}`;
    });

    expect(result.model).toBe("model-c");
    expect(result.result).toBe("result-from-model-c");
    expect(result.attempts).toBe(3);
    expect(attempts).toEqual(["model-a", "model-b", "model-c"]);
  });

  it("returns comprehensive error when all models exhausted", async () => {
    const { executeWithModelFallback } = await import("../../delegation");
    const { createModelFallbackChain } = await import("../../overlays");

    const chain = createModelFallbackChain(["model-a", "model-b", "model-c"]);

    const promise = executeWithModelFallback(chain.models, async (model) => {
      throw new Error(`${model} failed`);
    });

    await expect(promise).rejects.toThrow();

    try {
      await executeWithModelFallback(chain.models, async (model) => {
        throw new Error(`${model} failed`);
      });
    } catch (err) {
      expect(err).toBeInstanceOf(Error);
      const error = err as Error;
      // Error message should include details about all attempts
      expect(error.message).toContain("model-a");
      expect(error.message).toContain("model-b");
      expect(error.message).toContain("model-c");
      expect(error.message).toContain("3"); // attempt count
    }
  });

  it("works with a single model (no fallback chain)", async () => {
    const { executeWithModelFallback } = await import("../../delegation");
    const { createModelFallbackChain } = await import("../../overlays");

    const chain = createModelFallbackChain(["model-a"]);

    // Success case
    const result = await executeWithModelFallback(chain.models, async (model) => {
      return `result-from-${model}`;
    });
    expect(result.model).toBe("model-a");
    expect(result.result).toBe("result-from-model-a");
    expect(result.attempts).toBe(1);
  });

  it("single model failure throws original error", async () => {
    const { executeWithModelFallback } = await import("../../delegation");
    const { createModelFallbackChain } = await import("../../overlays");

    const chain = createModelFallbackChain(["model-a"]);

    try {
      await executeWithModelFallback(chain.models, async () => {
        throw new Error("model-a failed");
      });
      expect(true).toBe(false); // should not reach
    } catch (err) {
      expect(err).toBeInstanceOf(Error);
      expect((err as Error).message).toContain("model-a failed");
    }
  });

  it("preserves error details from each failed attempt", async () => {
    const { executeWithModelFallback } = await import("../../delegation");
    const { createModelFallbackChain } = await import("../../overlays");

    const chain = createModelFallbackChain(["model-a", "model-b"]);

    try {
      await executeWithModelFallback(chain.models, async (model) => {
        if (model === "model-a") throw new Error("rate limited");
        throw new Error("server error");
      });
    } catch (err) {
      const error = err as Error & { attempts?: Array<{ model: string; error: string }> };
      // Should have structured attempt details
      expect(error.attempts).toBeDefined();
      expect(error.attempts).toHaveLength(2);
      expect(error.attempts?.[0]?.model).toBe("model-a");
      expect(error.attempts?.[0]?.error).toContain("rate limited");
      expect(error.attempts?.[1]?.model).toBe("model-b");
      expect(error.attempts?.[1]?.error).toContain("server error");
    }
  });

  it("applies optional delay between attempts", async () => {
    const { executeWithModelFallback } = await import("../../delegation");
    const { createModelFallbackChain } = await import("../../overlays");

    const chain = createModelFallbackChain(["model-a", "model-b"]);
    const timestamps: number[] = [];

    const start = Date.now();
    await executeWithModelFallback(
      chain.models,
      async (model) => {
        timestamps.push(Date.now() - start);
        if (model === "model-a") throw new Error("fail");
        return "ok";
      },
      { delayMs: 50 }
    );

    // Second attempt should be at least 50ms after the first
    expect(timestamps.length).toBe(2);
    const first = timestamps[0] ?? 0;
    const second = timestamps[1] ?? 0;
    expect(second - first).toBeGreaterThanOrEqual(40); // allow 10ms tolerance
  });
});

// ─── getModelOverlay ─────────────────────────────────────────────────────────
// Fallback chain creation is separate via createModelFallbackChain.
// getModelOverlay returns overlays for a single model (providerID, modelID).

describe("getModelOverlay", () => {
  it("returns overlay for known provider", async () => {
    const { getModelOverlay } = await import("../../overlays");
    const overlay = getModelOverlay("anthropic", "claude-opus-4-6");
    expect(overlay).not.toBeNull();
    expect(overlay?.provider).toBe("anthropic");
  });

  it("returns null for unknown provider", async () => {
    const { getModelOverlay } = await import("../../overlays");
    const overlay = getModelOverlay("unknown-provider", "some-model");
    expect(overlay).toBeNull();
  });
});

// ─── Config: per-agent fallback chains ───────────────────────────────────────

describe("config: per-agent fallback chains", () => {
  it("AgentOverrideConfig supports fallbackModels array", async () => {
    const { loadPluginConfig } = await import("../../config");
    const fs = await import("node:fs");
    const os = await import("node:os");
    const path = await import("node:path");

    const tempDir = fs.mkdtempSync(path.join(os.tmpdir(), "fallback-test-"));
    const tempHome = fs.mkdtempSync(path.join(os.tmpdir(), "fallback-home-"));

    try {
      const configPath = path.join(tempDir, ".opencode", "opencode-legion.json");
      fs.mkdirSync(path.dirname(configPath), { recursive: true });
      fs.writeFileSync(
        configPath,
        JSON.stringify({
          agents: {
            oracle: {
              model: "anthropic/claude-opus-4-6",
              fallbackModels: ["openai/gpt-5.2-codex", "google/gemini-3-pro"],
            },
          },
        })
      );

      const config = await loadPluginConfig(tempDir, { homeDir: tempHome });
      const oracleConfig = config.agents?.oracle;
      expect(oracleConfig).toBeDefined();
      expect(oracleConfig?.fallbackModels).toEqual(["openai/gpt-5.2-codex", "google/gemini-3-pro"]);
    } finally {
      fs.rmSync(tempDir, { recursive: true, force: true });
      fs.rmSync(tempHome, { recursive: true, force: true });
    }
  });

  it("RetryConfig supports fallbackModels array", async () => {
    const { loadPluginConfig } = await import("../../config");
    const fs = await import("node:fs");
    const os = await import("node:os");
    const path = await import("node:path");

    const tempDir = fs.mkdtempSync(path.join(os.tmpdir(), "fallback-test-"));
    const tempHome = fs.mkdtempSync(path.join(os.tmpdir(), "fallback-home-"));

    try {
      const configPath = path.join(tempDir, ".opencode", "opencode-legion.json");
      fs.mkdirSync(path.dirname(configPath), { recursive: true });
      fs.writeFileSync(
        configPath,
        JSON.stringify({
          retry: {
            maxRetries: 2,
            delayMs: 1000,
            fallbackModels: ["openai/gpt-5.2-codex", "google/gemini-3-pro"],
          },
        })
      );

      const config = await loadPluginConfig(tempDir, { homeDir: tempHome });
      expect(config.retry?.fallbackModels).toEqual(["openai/gpt-5.2-codex", "google/gemini-3-pro"]);
    } finally {
      fs.rmSync(tempDir, { recursive: true, force: true });
      fs.rmSync(tempHome, { recursive: true, force: true });
    }
  });

  it("no fallback chain defined means behavior unchanged (single attempt)", async () => {
    const { executeWithModelFallback } = await import("../../delegation");
    const { createModelFallbackChain } = await import("../../overlays");

    // No fallback chain - just primary model
    const chain = createModelFallbackChain(["model-a"]);
    const attempts: string[] = [];

    try {
      await executeWithModelFallback(chain.models, async (model) => {
        attempts.push(model);
        throw new Error("fail");
      });
    } catch {
      // expected
    }

    expect(attempts).toEqual(["model-a"]);
  });

  it("different agents can have different fallback chains", async () => {
    const { loadPluginConfig } = await import("../../config");
    const fs = await import("node:fs");
    const os = await import("node:os");
    const path = await import("node:path");

    const tempDir = fs.mkdtempSync(path.join(os.tmpdir(), "fallback-test-"));
    const tempHome = fs.mkdtempSync(path.join(os.tmpdir(), "fallback-home-"));

    try {
      const configPath = path.join(tempDir, ".opencode", "opencode-legion.json");
      fs.mkdirSync(path.dirname(configPath), { recursive: true });
      fs.writeFileSync(
        configPath,
        JSON.stringify({
          agents: {
            oracle: {
              fallbackModels: ["openai/gpt-5.2-codex"],
            },
            executor: {
              fallbackModels: ["google/gemini-3-pro", "anthropic/claude-sonnet-4-6"],
            },
          },
        })
      );

      const config = await loadPluginConfig(tempDir, { homeDir: tempHome });
      expect(config.agents?.oracle?.fallbackModels).toEqual(["openai/gpt-5.2-codex"]);
      expect(config.agents?.executor?.fallbackModels).toEqual([
        "google/gemini-3-pro",
        "anthropic/claude-sonnet-4-6",
      ]);
    } finally {
      fs.rmSync(tempDir, { recursive: true, force: true });
      fs.rmSync(tempHome, { recursive: true, force: true });
    }
  });
});
