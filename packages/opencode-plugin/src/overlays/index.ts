import { getClaudeOverlay } from "./claude";

export type { ModelFallbackChain, ParsedModel } from "./fallback-chain";
export { createModelFallbackChain } from "./fallback-chain";

import type { ModelFallbackChain } from "./fallback-chain";
import { getGeminiOverlay } from "./gemini";
import { getGptOverlay } from "./gpt";
import type { ModelOverlay } from "./types";

export type { ModelOverlay } from "./types";

/**
 * Get the system prompt overlay for a given provider/model combination.
 *
 * @param providerID - Provider identifier (e.g. "anthropic", "openai", "google")
 * @param modelID - Model identifier within the provider
 * @param _fallbackChain - Optional fallback chain configuration (reserved for future use)
 */
export function getModelOverlay(
  providerID: string,
  modelID: string,
  _fallbackChain?: ModelFallbackChain
): ModelOverlay | null {
  if (providerID === "anthropic") return getClaudeOverlay(modelID);
  if (providerID === "openai") return getGptOverlay(modelID);
  if (providerID === "google") return getGeminiOverlay(modelID);
  return null;
}
