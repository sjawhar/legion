import type { ToolDefinition } from "@opencode-ai/plugin";
import { tool } from "@opencode-ai/plugin";
import { createAgents } from "../agents";
import type { PluginConfig } from "../config";
import type { BackgroundTaskManager } from "./background-manager";
import { resolveCategory } from "./category-router";

const z = tool.schema;

const DELEGATOR_ALLOWLIST = new Set(["orchestrator", "conductor"]);

interface DelegationToolContext {
  agent?: string;
  sessionID?: string;
}

export function createDelegationTools(
  manager: BackgroundTaskManager,
  config?: PluginConfig
): Record<string, ToolDefinition> {
  const agentModelMap = new Map(
    createAgents(config).map((agent) => [agent.name, agent.config.model])
  );
  const background_task = tool({
    description:
      "Delegate work to a specialist agent running in background. " +
      "Returns task_id immediately. Use background_output to get results.",
    args: {
      prompt: z.string().describe("Task prompt for the agent"),
      category: z
        .string()
        .optional()
        .describe(
          "Category: ultrabrain, deep, visual-engineering, artistry, " +
            "quick, writing, unspecified-low, unspecified-high"
        ),
      model: z.string().optional().describe("Model override (e.g. 'anthropic/claude-opus-4-6')"),
      subagent_type: z
        .string()
        .optional()
        .describe("Specific agent name (e.g. executor, explorer, oracle)"),
      description: z.string().describe("Short task description (5-10 words)"),
      run_in_background: z.boolean().optional().default(true),
    },
    async execute(args, toolContext) {
      const context = toolContext as DelegationToolContext | undefined;
      const callingAgent = context?.agent?.toLowerCase();
      if (!callingAgent || !DELEGATOR_ALLOWLIST.has(callingAgent)) {
        return `Error: Agent '${callingAgent ?? "unknown"}' cannot delegate tasks. Only orchestrator-type agents can use background_task.`;
      }

      const category = args.category as string | undefined;
      const modelOverride = args.model as string | undefined;
      const categoryConfig = category
        ? resolveCategory(category, config?.categories, modelOverride)
        : undefined;
      if (category && (!categoryConfig?.model || categoryConfig.model.trim().length === 0)) {
        return `Error: Category '${category}' has no model configured.`;
      }

      const agentName = (args.subagent_type as string | undefined) ?? "executor";
      if (!agentModelMap.has(agentName)) {
        const known = [...agentModelMap.keys()].join(", ");
        return `Error: Unknown agent '${agentName}'. Available agents: ${known}`;
      }
      if (DELEGATOR_ALLOWLIST.has(agentName.toLowerCase())) {
        return `Error: Cannot delegate to '${agentName}' — delegator agents cannot be delegation targets.`;
      }
      const resolvedModel = modelOverride ?? categoryConfig?.model ?? agentModelMap.get(agentName);

      const parentSessionId = context?.sessionID;

      const task = await manager.launch({
        agent: agentName,
        prompt: args.prompt as string,
        description: args.description as string,
        model: resolvedModel,
        parentSessionId,
        systemPrompt: categoryConfig?.systemPrompt,
      });

      return [
        "Session created.",
        "",
        `Session ID: ${task.sessionID ?? "pending"}`,
        `Task ID: ${task.id}`,
        `Agent: ${task.agent}`,
        `Model: ${task.model}`,
        "",
        `Attach: opencode attach ${task.sessionID ?? task.id}`,
        `Output: Use background_output with task_id="${task.id}"`,
      ].join("\n");
    },
  });

  const background_output = tool({
    description:
      "Get output from a background task. " +
      "Returns results if completed, status if still running.",
    args: {
      task_id: z.string().describe("Task ID from background_task"),
    },
    async execute(args) {
      return await manager.getTaskOutput(args.task_id as string);
    },
  });

  const background_cancel = tool({
    description:
      "Cancel background task(s). " +
      "Provide task_id for one, or all=true to cancel all running tasks.",
    args: {
      task_id: z.string().optional().describe("Specific task to cancel"),
      all: z.boolean().optional().describe("Cancel all running tasks"),
    },
    async execute(args) {
      if (args.all === true) {
        const count = await manager.cancelAll();
        return `Cancelled ${count} task(s).`;
      }
      const taskId = args.task_id as string | undefined;
      if (taskId) {
        return (await manager.cancel(taskId))
          ? `Cancelled task ${taskId}.`
          : `Task ${taskId} not found or not running.`;
      }
      return "Specify task_id or use all=true.";
    },
  });

  return { background_task, background_output, background_cancel };
}
