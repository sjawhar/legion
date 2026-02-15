/// <reference lib="es2015" />
import type { Plugin } from "@opencode-ai/plugin";
import type { Event } from "@opencode-ai/sdk";
import { createAgents } from "./agents";
import type { PermissionConfig } from "./config";
import { loadPluginConfig, mergePermissionConfig } from "./config";
import { BackgroundTaskManager, createDelegationTools } from "./delegation";
import { anthropicEffortHook } from "./hooks/anthropic-effort";
import { createBackgroundNotificationHook } from "./hooks/background-notification";
import { COMPACTION_CONTEXT_TEMPLATE } from "./hooks/compaction-context-injector";
import { createCompactionTodoPreserverHook } from "./hooks/compaction-todo-preserver";
import { nonInteractiveEnvHook } from "./hooks/non-interactive-env";
import { createPreemptiveCompactionHook } from "./hooks/preemptive-compaction";
import { createSessionRecoveryHook } from "./hooks/session-recovery";
import { createStopContinuationGuardHook } from "./hooks/stop-continuation-guard";
import { subagentQuestionBlockerHook } from "./hooks/subagent-question-blocker";
import { thinkingBlockValidatorHook } from "./hooks/thinking-block-validator";
import { createTodoContinuationEnforcerHook } from "./hooks/todo-continuation-enforcer";
import { isRecord, resolveSessionID } from "./hooks/utils";
import { getModelOverlay } from "./overlays";
import { createSessionTools } from "./tools";
import { createTaskTools } from "./tools/task";

const DEFAULT_GLOBAL_PERMISSION: PermissionConfig = {
  read: "allow",
  glob: "allow",
  list: "allow",
  edit: "allow",
  bash: "allow",
  task: "allow",
};

interface AgentConfigEntry {
  model: string;
  temperature: number;
  prompt: string;
  description: string;
  permission?: PermissionConfig;
}

interface OpencodeConfigShape {
  default_agent?: string;
  permission?: PermissionConfig;
  agent?: Record<string, AgentConfigEntry | Record<string, unknown>>;
}

interface GenericEventInput {
  event: { type: string; properties?: unknown };
}

interface ToolExecuteInput {
  tool: string;
  sessionID?: string;
}

interface SlashCommandOutput {
  args?: { command?: string };
}

interface ChatMessageInput {
  sessionID?: string;
}

interface SessionCompactingInput {
  sessionID: string;
}

interface SessionCompactingOutput {
  context: string[];
}

const OpenCodeLegion: Plugin = async (ctx) => {
  const pluginConfig = await loadPluginConfig(ctx.directory);
  const manager = new BackgroundTaskManager(ctx);
  const delegationTools = createDelegationTools(manager, pluginConfig);
  const sessionTools = createSessionTools(ctx.client, ctx.directory);
  const taskTools = createTaskTools(ctx);
  const backgroundNotificationHook = createBackgroundNotificationHook((sessionID, status) => {
    if (manager.isBackgroundSession(sessionID)) {
      console.log(`[opencode-legion] Background task ${sessionID} ${status}`);
    }
  });
  const preemptiveCompactionHook = createPreemptiveCompactionHook(ctx);
  const sessionRecoveryHook = createSessionRecoveryHook(ctx);
  const stopContinuationGuardHook = createStopContinuationGuardHook();
  const compactionTodoPreserver = createCompactionTodoPreserverHook(ctx);
  const todoContinuationEnforcer = createTodoContinuationEnforcerHook(ctx, {
    isContinuationStopped: (sessionID) => stopContinuationGuardHook.isStopped(sessionID),
    isBackgroundSession: (sessionID) => manager.isBackgroundSession(sessionID),
    isRecovering: (sessionID) => sessionRecoveryHook.isRecovering(sessionID),
    gracePeriodMs: pluginConfig.continuationGracePeriodMs,
  });

  return {
    name: "opencode-legion",
    config: async (opencodeConfig: Record<string, unknown>) => {
      const agents = createAgents(pluginConfig);
      const agentMap: Record<string, AgentConfigEntry> = {};
      for (const agent of agents) {
        agentMap[agent.name] = {
          model: agent.config.model,
          temperature: agent.config.temperature,
          prompt: agent.config.prompt,
          description: agent.description,
        };
      }

      const config = opencodeConfig as OpencodeConfigShape;
      config.default_agent = "orchestrator";

      const existingPermission = config.permission;
      const mergedGlobalPermission = mergePermissionConfig(
        DEFAULT_GLOBAL_PERMISSION,
        pluginConfig.permission
      );
      config.permission = mergePermissionConfig(mergedGlobalPermission, existingPermission);

      if (!config.agent) {
        config.agent = agentMap;
      } else {
        Object.assign(config.agent, agentMap);
      }

      const conductorEntry = config.agent?.conductor as Record<string, unknown> | undefined;
      if (conductorEntry) {
        conductorEntry.permission = {
          read: "allow",
          glob: "allow",
          list: "allow",
          edit: "deny",
          write: "deny",
          bash: "deny",
          task: "allow",
        };
      }
    },
    event: async (input: { event: Event }) => {
      await manager.handleSessionStatus(input.event);
      backgroundNotificationHook(input);
      await preemptiveCompactionHook.event?.(input as GenericEventInput);
      await stopContinuationGuardHook.event(input as GenericEventInput);
      await compactionTodoPreserver.event(input as GenericEventInput);
      await todoContinuationEnforcer.event(input as GenericEventInput);

      const { event } = input;

      if (event.type === "session.deleted") {
        const sessionProps = isRecord(event.properties) ? event.properties : undefined;
        const sessionID = resolveSessionID(sessionProps);
        if (sessionID) {
          await manager.cleanup(sessionID);
        }
      }

      if (event.type === "session.error") {
        const props = isRecord(event.properties)
          ? (event.properties as Record<string, unknown>)
          : undefined;
        const sessionID = resolveSessionID(props);
        const error = props?.error;
        if (sessionRecoveryHook.isRecoverableError(error)) {
          const messageInfo = {
            id: typeof props?.messageID === "string" ? props.messageID : undefined,
            role: "assistant" as const,
            sessionID,
            error,
          };
          await sessionRecoveryHook.handleSessionRecovery(messageInfo);
        }
      }
    },
    tool: {
      ...delegationTools,
      ...sessionTools,
      ...taskTools,
    },
    "chat.params": async (input, output) => {
      anthropicEffortHook(input, output);
    },
    "tool.execute.before": async (input, output) => {
      subagentQuestionBlockerHook(input, output);
      const toolInput = isRecord(input) ? (input as ToolExecuteInput) : undefined;
      const toolName = typeof toolInput?.tool === "string" ? toolInput.tool : undefined;
      const sessionID = typeof toolInput?.sessionID === "string" ? toolInput.sessionID : undefined;
      if (toolName === "slashcommand") {
        const outputRecord = isRecord(output) ? (output as SlashCommandOutput) : undefined;
        const args = outputRecord?.args;
        const commandValue = typeof args?.command === "string" ? args.command : undefined;
        const command = commandValue?.replace(/^\//, "").toLowerCase();
        if (command === "stop-continuation" && sessionID) {
          stopContinuationGuardHook.stop(sessionID);
          todoContinuationEnforcer.cancelPending(sessionID);
        }
      }
    },
    "tool.execute.after": async (input, output) => {
      await preemptiveCompactionHook["tool.execute.after"]?.(input, output);
    },
    "chat.message": async (input) => {
      await stopContinuationGuardHook["chat.message"](input as ChatMessageInput);
      await todoContinuationEnforcer.chatMessage(input as ChatMessageInput);
    },
    "shell.env": async (input, output) => {
      nonInteractiveEnvHook(input, output);
    },
    "experimental.chat.messages.transform": async (input, output) => {
      thinkingBlockValidatorHook(input, output);
    },
    "experimental.session.compacting": async (
      _input: SessionCompactingInput,
      output: SessionCompactingOutput
    ): Promise<void> => {
      await compactionTodoPreserver.capture(_input.sessionID);
      output.context.push(COMPACTION_CONTEXT_TEMPLATE);
    },
    "experimental.chat.system.transform": async (input, output) => {
      const overlay = getModelOverlay(input.model.providerID, input.model.id);
      if (overlay && !output.system.includes(overlay.systemContent)) {
        output.system.push(overlay.systemContent);
      }
    },
  };
};

export default OpenCodeLegion;
