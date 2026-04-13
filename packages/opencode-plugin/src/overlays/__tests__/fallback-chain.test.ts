/**
 * Tests for createModelFallbackChain — model fallback chain creation (T7/T6).
 *
 * Part of #276 / #200: OMO Replacement.
 *
 * Tests cover:
 * - Creating fallback chains from ordered model lists
 * - Iterating through models in order
 * - Empty chain handling
 * - Single model chain (no fallback)
 * - getModelOverlay with fallback chain config (3rd param)
 */

import { describe, expect, it } from "bun:test";
import { createModelFallbackChain } from "../fallback-chain";
import { getModelOverlay } from "../index";

describe("createModelFallbackChain", () => {
  it("creates a chain from an ordered list of models", () => {
    const chain = createModelFallbackChain([
      "anthropic/claude-sonnet-4-20250514",
      "openai/gpt-5.3-codex",
      "google/gemini-3-pro",
    ]);

    expect(chain.models).toEqual([
      "anthropic/claude-sonnet-4-20250514",
      "openai/gpt-5.3-codex",
      "google/gemini-3-pro",
    ]);
    expect(chain.length).toBe(3);
  });

  it("returns primary model as first in chain", () => {
    const chain = createModelFallbackChain(["anthropic/claude-opus-4-6", "openai/gpt-5.3-codex"]);

    expect(chain.primary()).toBe("anthropic/claude-opus-4-6");
  });

  it("iterates through fallback models in order", () => {
    const chain = createModelFallbackChain([
      "anthropic/claude-sonnet-4-20250514",
      "openai/gpt-5.3-codex",
      "google/gemini-3-pro",
    ]);

    const fallbacks = chain.fallbacks();
    expect(fallbacks).toEqual(["openai/gpt-5.3-codex", "google/gemini-3-pro"]);
  });

  it("returns empty fallbacks for single model chain", () => {
    const chain = createModelFallbackChain(["anthropic/claude-sonnet-4-20250514"]);

    expect(chain.primary()).toBe("anthropic/claude-sonnet-4-20250514");
    expect(chain.fallbacks()).toEqual([]);
    expect(chain.length).toBe(1);
  });

  it("handles empty model list gracefully", () => {
    const chain = createModelFallbackChain([]);

    expect(chain.primary()).toBeUndefined();
    expect(chain.fallbacks()).toEqual([]);
    expect(chain.length).toBe(0);
  });

  it("splits model string into providerID and modelID", () => {
    const chain = createModelFallbackChain(["anthropic/claude-sonnet-4-20250514"]);
    const parsed = chain.parsePrimary();

    expect(parsed).toEqual({
      providerID: "anthropic",
      modelID: "claude-sonnet-4-20250514",
    });
  });

  it("parsePrimary returns undefined for empty chain", () => {
    const chain = createModelFallbackChain([]);
    expect(chain.parsePrimary()).toBeUndefined();
  });

  it("parseModel splits any model string into provider/model", () => {
    const chain = createModelFallbackChain(["openai/gpt-5.3-codex"]);
    const parsed = chain.parseModel("google/gemini-3-pro");

    expect(parsed).toEqual({
      providerID: "google",
      modelID: "gemini-3-pro",
    });
  });

  it("parseModel returns model as-is when no slash present", () => {
    const chain = createModelFallbackChain([]);
    const parsed = chain.parseModel("some-model");

    expect(parsed).toEqual({
      providerID: "some-model",
      modelID: "some-model",
    });
  });

  it("creates chain from primary model + fallbackModel config", () => {
    const chain = createModelFallbackChain(
      ["anthropic/claude-sonnet-4-20250514"],
      "openai/gpt-5.3-codex"
    );

    expect(chain.models).toEqual(["anthropic/claude-sonnet-4-20250514", "openai/gpt-5.3-codex"]);
    expect(chain.length).toBe(2);
  });

  it("does not duplicate if fallbackModel is already in list", () => {
    const chain = createModelFallbackChain(
      ["anthropic/claude-sonnet-4-20250514", "openai/gpt-5.3-codex"],
      "openai/gpt-5.3-codex"
    );

    expect(chain.models).toEqual(["anthropic/claude-sonnet-4-20250514", "openai/gpt-5.3-codex"]);
    expect(chain.length).toBe(2);
  });
});

describe("getModelOverlay with fallback chain config", () => {
  it("accepts a third parameter for fallback chain configuration", () => {
    // The contract test requires getModelOverlay.length >= 3
    expect(getModelOverlay.length).toBeGreaterThanOrEqual(3);
  });

  it("returns overlay for primary model when no fallback config", () => {
    const overlay = getModelOverlay("anthropic", "claude-sonnet-4-20250514");
    // Should still work as before with 2 args
    expect(overlay).toBeDefined();
    expect(overlay?.provider).toBe("anthropic");
  });

  it("returns overlay for primary model when fallback config provided", () => {
    const chain = createModelFallbackChain([
      "anthropic/claude-sonnet-4-20250514",
      "openai/gpt-5.3-codex",
    ]);
    const overlay = getModelOverlay("anthropic", "claude-sonnet-4-20250514", chain);
    expect(overlay).toBeDefined();
    expect(overlay?.provider).toBe("anthropic");
  });
});
