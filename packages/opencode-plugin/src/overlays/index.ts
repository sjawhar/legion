import { getClaudeOverlay } from "./claude";
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
