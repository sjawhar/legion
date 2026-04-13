import { getClaudeOverlay } from "./claude";
import { getGeminiOverlay } from "./gemini";
import { getGptOverlay } from "./gpt";
import type { FallbackChain, ModelOverlay } from "./types";

export type { FallbackChain, ModelOverlay } from "./types";

export function getModelOverlay(providerID: string, modelID: string): ModelOverlay | null {
  if (providerID === "anthropic") return getClaudeOverlay(modelID);
  if (providerID === "openai") return getGptOverlay(modelID);
  if (providerID === "google") return getGeminiOverlay(modelID);
  return null;
}

/**
 * Create an ordered fallback chain of models to try in sequence.
 * The primary model is tried first; on failure, each fallback is tried in order.
 * Duplicate models are removed (preserving first occurrence).
 */
export function createModelFallbackChain(primary: string, fallbacks?: string[]): FallbackChain {
  const seen = new Set<string>([primary]);
  const dedupedFallbacks: string[] = [];
  if (fallbacks) {
    for (const model of fallbacks) {
      if (!seen.has(model)) {
        seen.add(model);
        dedupedFallbacks.push(model);
      }
    }
  }

  return {
    primary,
    fallbacks: dedupedFallbacks,
    [Symbol.iterator](): Iterator<string> {
      const all = [primary, ...dedupedFallbacks];
      let index = 0;
      return {
        next(): IteratorResult<string> {
          const value = all[index];
          if (index < all.length && value !== undefined) {
            index++;
            return { value, done: false };
          }
          return { value: undefined as unknown as string, done: true };
        },
      };
    },
  };
}
