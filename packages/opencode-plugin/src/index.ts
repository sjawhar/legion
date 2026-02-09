/// <reference lib="es2015" />
import type { Plugin } from "@opencode-ai/plugin";
import type { Event } from "@opencode-ai/sdk";
import { createAgents } from "./agents";
import type { PermissionConfig } from "./config";
import { loadPluginConfig, mergePermissionConfig } from "./config";
import { BackgroundTaskManager, createDelegationTools } from "./delegation";
import {
  anthropicEffortHook,
  createBackgroundNotificationHook,
  createPreemptiveCompactionHook,
  createSessionRecoveryHook,
  createStopContinuationGuardHook,
  nonInteractiveEnvHook,
  subagentQuestionBlockerHook,
  thinkingBlockValidatorHook,
} from "./hooks";
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

  return {
    name: "opencode-legion",
    config: async (opencodeConfig: Record<string, unknown>) => {
      const agents = createAgents(pluginConfig);
      const agentMap: Record<
        string,
        {
          model: string;
          temperature: number;
          prompt: string;
          description: string;
          permission?: PermissionConfig;
        }
      > = {};
      for (const agent of agents) {
        agentMap[agent.name] = {
          model: agent.config.model,
          temperature: agent.config.temperature,
          prompt: agent.config.prompt,
          description: agent.description,
        };
      }

      (opencodeConfig as { default_agent?: string }).default_agent = "orchestrator";

      const existingPermission = (opencodeConfig as { permission?: PermissionConfig }).permission;
      const mergedGlobalPermission = mergePermissionConfig(
        DEFAULT_GLOBAL_PERMISSION,
        pluginConfig.permission
      );
      (opencodeConfig as { permission?: PermissionConfig }).permission = mergePermissionConfig(
        mergedGlobalPermission,
        existingPermission
      );

      if (!opencodeConfig.agent) {
        opencodeConfig.agent = agentMap;
      } else {
        Object.assign(opencodeConfig.agent, agentMap);
      }
    },
    event: async (input: { event: Event }) => {
      manager.handleSessionStatus(input.event);
      backgroundNotificationHook(input);
      await preemptiveCompactionHook.event?.(
        input as { event: { type: string; properties?: unknown } }
      );
      await stopContinuationGuardHook.event(
        input as { event: { type: string; properties?: unknown } }
      );

      const { event } = input;

      if (event.type === "session.deleted") {
        const delProps = event.properties as Record<string, unknown> | undefined;
        const delInfo = delProps?.info as { id?: string } | undefined;
        if (delInfo?.id) {
          manager.cleanup(delInfo.id);
        }
      }

      if (event.type === "session.error") {
        const props = event.properties as Record<string, unknown> | undefined;
        const sessionID = props?.sessionID as string | undefined;
        const error = props?.error;
        if (sessionRecoveryHook.isRecoverableError(error)) {
          const messageInfo = {
            id: props?.messageID as string | undefined,
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
    },
    "tool.execute.after": async (input, output) => {
      await preemptiveCompactionHook["tool.execute.after"]?.(input, output);
    },
    "chat.message": async (input) => {
      await stopContinuationGuardHook["chat.message"](input as { sessionID?: string });
    },
    "shell.env": async (input, output) => {
      nonInteractiveEnvHook(input, output);
    },
    "experimental.chat.messages.transform": async (input, output) => {
      thinkingBlockValidatorHook(input, output);
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
