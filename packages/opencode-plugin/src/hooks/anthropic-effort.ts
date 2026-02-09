import type { ProviderContext } from "@opencode-ai/plugin";
import type { Model, UserMessage } from "@opencode-ai/sdk";

const OPUS_PATTERN = /claude-opus-4[-.]6/i;

function normalizeModelID(modelID: string): string {
  return modelID.replace(/\.(\d+)/g, "-$1");
}

function isClaudeProvider(providerID: string, modelID: string): boolean {
  if (["anthropic", "opencode"].includes(providerID)) return true;
  if (providerID === "github-copilot" && modelID.toLowerCase().includes("claude")) return true;
  return false;
}

function isOpus46(modelID: string): boolean {
  return OPUS_PATTERN.test(normalizeModelID(modelID));
}

interface ChatParamsInput {
  sessionID: string;
  agent: string;
  model: Model;
  provider: ProviderContext;
  message: UserMessage;
}

interface ChatParamsOutput {
  temperature: number;
  topP: number;
  topK: number;
  options: Record<string, unknown>;
}

export function anthropicEffortHook(input: ChatParamsInput, output: ChatParamsOutput): void {
  const { model } = input;
  if (!model?.id || !model?.providerID) return;
  if ((input as { variant?: string }).variant !== "max") return;
  if (!isClaudeProvider(model.providerID, model.id)) return;
  if (!isOpus46(model.id)) return;
  if (output.options.effort !== undefined) return;

  output.options.effort = "max";
}
