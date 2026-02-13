import type { PluginInput } from "@opencode-ai/plugin";

const PREEMPTIVE_COMPACTION_THRESHOLD = 0.78;
const PROVIDER_LIMITS: Record<string, number> = {
  anthropic: 200_000,
  openai: 128_000,
  google: 1_000_000,
};

interface AssistantMessageInfo {
  role: "assistant";
  providerID: string;
  modelID?: string;
  tokens: {
    input: number;
    output: number;
    reasoning: number;
    cache: { read: number; write: number };
  };
}

interface MessageWrapper {
  info: { role: string } & Partial<AssistantMessageInfo>;
}

function getProviderLimit(providerID?: string): number {
  if (!providerID) return PROVIDER_LIMITS.openai;
  return PROVIDER_LIMITS[providerID] ?? PROVIDER_LIMITS.openai;
}

export function createPreemptiveCompactionHook(ctx: PluginInput) {
  const compactionInProgress = new Set<string>();

  const toolExecuteAfter = async (
    input: { tool: string; sessionID: string; callID: string },
    _output: { title: string; output: string; metadata: unknown }
  ) => {
    const { sessionID } = input;
    if (compactionInProgress.has(sessionID)) return;

    try {
      const response = await ctx.client.session.messages({
        path: { id: sessionID },
        query: { directory: ctx.directory },
      });
      const payload = response as { data?: MessageWrapper[] } | MessageWrapper[];
      const messages = Array.isArray(payload) ? payload : (payload.data ?? []);
      const assistantMessages = messages
        .filter((m) => m.info.role === "assistant")
        .map((m) => m.info as AssistantMessageInfo);

      if (assistantMessages.length === 0) return;

      const lastAssistant = assistantMessages[assistantMessages.length - 1];
      const actualLimit = getProviderLimit(lastAssistant.providerID);

      const lastTokens = lastAssistant.tokens;
      const totalInputTokens = (lastTokens?.input ?? 0) + (lastTokens?.cache?.read ?? 0);
      const usageRatio = totalInputTokens / actualLimit;

      if (usageRatio < PREEMPTIVE_COMPACTION_THRESHOLD) return;

      const modelID = lastAssistant.modelID;
      if (!modelID) return;

      compactionInProgress.add(sessionID);

      await ctx.client.session.summarize({
        path: { id: sessionID },
        body: { providerID: lastAssistant.providerID, modelID, auto: true } as never,
        query: { directory: ctx.directory },
      });
    } catch {
      // best-effort; do not disrupt tool execution
    } finally {
      compactionInProgress.delete(sessionID);
    }
  };

  const eventHandler = async ({ event }: { event: { type: string; properties?: unknown } }) => {
    if (event.type !== "session.deleted") return;
    const props = event.properties as Record<string, unknown> | undefined;
    const sessionInfo = props?.info as { id?: string } | undefined;
    if (sessionInfo?.id) {
      compactionInProgress.delete(sessionInfo.id);
    }
  };

  return {
    "tool.execute.after": toolExecuteAfter,
    event: eventHandler,
  };
}
