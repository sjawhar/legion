import { getClaudeOverlay } from "./claude";

export type { ModelFallbackChain, ParsedModel } from "./fallback-chain";
export { createModelFallbackChain } from "./fallback-chain";

import { getGeminiOverlay } from "./gemini";
import { getGptOverlay } from "./gpt";
import type { ModelOverlay } from "./types";

export type { ModelOverlay } from "./types";

export function getModelOverlay(providerID: string, modelID: string): ModelOverlay | null {
  if (providerID === "anthropic") return getClaudeOverlay(modelID);
  if (providerID === "openai") return getGptOverlay(modelID);
  if (providerID === "google") return getGeminiOverlay(modelID);
  return null;
}
