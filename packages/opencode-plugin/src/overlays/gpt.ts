import type { ModelOverlay } from "./types";

const GPT_OVERLAY = [
  "<model-guidance>",
  "You are running as a GPT model. These behavioral hints optimize your output:",
  "",
  "SCOPE: Implement exactly what is asked — nothing more. Do not add helper functions, extra error handling, or features not explicitly requested. Scope creep is the primary failure mode to avoid.",
  "",
  "BREVITY: Keep responses short and action-oriented. No introductory summaries, no restating the task, no closing summaries. Answer, then stop.",
  "",
  "CONSISTENCY: If the system prompt contains rules, follow them literally. Never resolve ambiguity between instructions by picking one — flag the conflict instead.",
  "",
  "TOOL USE: Use tools when available rather than reasoning from memory. Execute tool calls immediately; do not narrate what you plan to do before doing it.",
  "</model-guidance>",
].join("\n");

const GPT_REASONING_OVERLAY = [
  "<model-guidance>",
  "You are running as a reasoning model. Keep supplemental instructions minimal — your reasoning capabilities handle complexity natively. Execute the task directly. Use tools over prose.",
  "</model-guidance>",
].join("\n");

function isReasoningModel(modelID: string): boolean {
  return /\bo[1-9]/.test(modelID) || modelID.includes("o1-") || modelID.includes("o3-");
}

export function getGptOverlay(modelID: string): ModelOverlay {
  return {
    systemContent: isReasoningModel(modelID) ? GPT_REASONING_OVERLAY : GPT_OVERLAY,
    provider: "openai",
  };
}
