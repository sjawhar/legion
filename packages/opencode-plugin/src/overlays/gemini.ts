import type { ModelOverlay } from "./types";

const GEMINI_OVERLAY = [
  "<model-guidance>",
  "You are running as a Gemini model. These behavioral hints optimize your output:",
  "",
  "STRUCTURE: Prefer structured output (lists, tables, code blocks) over prose paragraphs. When returning data, use the most machine-parseable format appropriate.",
  "",
  "CONCISENESS: Be direct. Lead with the answer or action. Omit preambles and restatements.",
  "",
  "TOOL USE: Use tools when available. Batch independent calls. Do not narrate tool plans — execute them.",
  "</model-guidance>",
].join("\n");

export function getGeminiOverlay(_modelID: string): ModelOverlay {
  return {
    systemContent: GEMINI_OVERLAY,
    provider: "google",
  };
}
