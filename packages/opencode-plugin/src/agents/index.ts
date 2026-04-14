import type { AgentOverrideConfig, PluginConfig } from "../config";
import { createConductorAgent } from "./conductor";
import { createExecutorAgent } from "./executor";
import { createExplorerAgent } from "./explorer";
import { createLibrarianAgent } from "./librarian";
import { createMetisAgent } from "./metis";
import { createMomusAgent } from "./momus";
import { createMultimodalAgent } from "./multimodal";
import { createOracleAgent } from "./oracle";
import { createOrchestratorAgent } from "./orchestrator";
import { createSimplicityReviewerAgent } from "./simplicity-reviewer";
import type { AgentDefinition } from "./types";

export type { AgentDefinition } from "./types";

const DEFAULT_MODELS: Record<string, string> = {
  orchestrator: "anthropic/claude-opus-4-6",
  executor: "openai/gpt-5.4",
  oracle: "openai/gpt-5.4",
  explorer: "anthropic/claude-haiku-4-5",
  librarian: "anthropic/claude-sonnet-4-6",
  metis: "openai/gpt-5.4",
  momus: "openai/gpt-5.4",
  multimodal: "openai/gpt-5.4",
  conductor: "anthropic/claude-opus-4-6",
  "simplicity-reviewer": "openai/gpt-5.4",
};

function getModel(config: PluginConfig | undefined, agentName: string): string {
  return config?.agents?.[agentName]?.model ?? DEFAULT_MODELS[agentName] ?? "";
}

function applyOverrides(
  agent: AgentDefinition,
  override: AgentOverrideConfig | undefined
): AgentDefinition {
  if (!override) return agent;
  const updated = {
    ...agent,
    config: { ...agent.config },
  };
  if (override.model) {
    updated.config.model = override.model;
  }
  if (typeof override.temperature === "number") {
    updated.config.temperature = override.temperature;
  }
  return updated;
}

export function createAgents(config?: PluginConfig): AgentDefinition[] {
  const agents = [
    createOrchestratorAgent(getModel(config, "orchestrator")),
    createExecutorAgent(getModel(config, "executor")),
    createOracleAgent(getModel(config, "oracle")),
    createExplorerAgent(getModel(config, "explorer")),
    createLibrarianAgent(getModel(config, "librarian")),
    createMetisAgent(getModel(config, "metis")),
    createMomusAgent(getModel(config, "momus")),
    createMultimodalAgent(getModel(config, "multimodal")),
    createConductorAgent(getModel(config, "conductor")),
    createSimplicityReviewerAgent(getModel(config, "simplicity-reviewer")),
  ];

  return agents.map((agent) => applyOverrides(agent, config?.agents?.[agent.name]));
}
