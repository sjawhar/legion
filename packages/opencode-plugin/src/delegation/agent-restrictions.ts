/**
 * Centralized agent tool restrictions.
 * Defines which tools are available to each agent type.
 * Used by startPrompt() to restrict tools at the SDK level (defense-in-depth).
 */

interface ToolRestrictions {
  write?: boolean;
  edit?: boolean;
  background_task?: boolean;
  background_cancel?: boolean;
}

const DEFAULT_RESTRICTIONS: ToolRestrictions = {
  write: false,
  edit: false,
  background_task: false,
  background_cancel: false,
};

const LEAF_AGENTS = ["explore", "explorer", "librarian", "oracle", "metis", "momus"];
const NO_DELEGATION_AGENTS = ["multimodal", "multimodal-looker", "simplicity-reviewer", "executor"];

const AGENT_RESTRICTIONS: Record<string, ToolRestrictions> = {
  ...Object.fromEntries(LEAF_AGENTS.map((name) => [name, DEFAULT_RESTRICTIONS])),
  ...Object.fromEntries(
    NO_DELEGATION_AGENTS.map((name) => [name, { background_task: false, background_cancel: false }])
  ),
  orchestrator: {},
  conductor: {
    write: false,
    edit: false,
  },
};

/**
 * Get tool restrictions for an agent.
 * Case-insensitive matching.
 * Unknown agents get default restrictions (fail-closed by default).
 */
export function getAgentToolRestrictions(agentName: string): Record<string, boolean> {
  const normalized = agentName.toLowerCase();
  return { ...(AGENT_RESTRICTIONS[normalized] ?? DEFAULT_RESTRICTIONS) } as Record<string, boolean>;
}

/**
 * Check if an agent is a leaf agent (cannot delegate).
 * Leaf agents have background_task: false in their restrictions.
 */
export function isLeafAgent(agentName: string): boolean {
  const restrictions = getAgentToolRestrictions(agentName);
  return restrictions.background_task === false;
}
