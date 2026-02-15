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

const AGENT_RESTRICTIONS: Record<string, ToolRestrictions> = {
  explore: {
    write: false,
    edit: false,
    background_task: false,
    background_cancel: false,
  },
  explorer: {
    write: false,
    edit: false,
    background_task: false,
    background_cancel: false,
  },
  librarian: {
    write: false,
    edit: false,
    background_task: false,
    background_cancel: false,
  },
  oracle: {
    write: false,
    edit: false,
    background_task: false,
    background_cancel: false,
  },
  metis: {
    write: false,
    edit: false,
    background_task: false,
    background_cancel: false,
  },
  momus: {
    write: false,
    edit: false,
    background_task: false,
    background_cancel: false,
  },
  multimodal: {
    background_task: false,
    background_cancel: false,
  },
  "multimodal-looker": {
    background_task: false,
    background_cancel: false,
  },
  "simplicity-reviewer": {
    background_task: false,
    background_cancel: false,
  },
  executor: {
    background_task: false,
    background_cancel: false,
  },
};

/**
 * Get tool restrictions for an agent.
 * Case-insensitive matching.
 * Unknown agents get empty restrictions (open by default).
 */
export function getAgentToolRestrictions(agentName: string): Record<string, boolean> {
  const normalized = agentName.toLowerCase();
  return (AGENT_RESTRICTIONS[normalized] ?? {}) as Record<string, boolean>;
}

/**
 * Check if an agent is a leaf agent (cannot delegate).
 * Leaf agents have background_task: false in their restrictions.
 */
export function isLeafAgent(agentName: string): boolean {
  const restrictions = getAgentToolRestrictions(agentName);
  return restrictions.background_task === false;
}
