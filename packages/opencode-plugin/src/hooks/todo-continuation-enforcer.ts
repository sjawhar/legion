import type { PluginInput } from "@opencode-ai/plugin";
import { extractTodos, isRecord, resolveSessionID, type TodoItem } from "./utils";

const CONTINUATION_AGENTS = new Set(["orchestrator", "executor", "builder", "conductor"]);
const DEFAULT_GRACE_PERIOD_MS = 2000;

const CONTINUATION_PROMPT =
  "Continue working on the next incomplete task. Pick up where you left off.";

interface MessageInfo {
  role?: string;
  agent?: string;
  providerID?: string;
  modelID?: string;
  model?: { providerID?: string; modelID?: string };
}

interface SessionMessage {
  info?: MessageInfo;
}

interface SessionEventInput {
  event: { type: string; properties?: unknown };
}

interface ChatMessageInput {
  sessionID?: string;
}

interface ResolvedAgent {
  agent: string;
  model?: { providerID: string; modelID: string };
}

function getIncompleteCount(todos: TodoItem[]): number {
  return todos.filter((t) => t.status !== "completed" && t.status !== "cancelled").length;
}

function resolveAgentFromMessages(messages: SessionMessage[]): ResolvedAgent | undefined {
  for (let i = messages.length - 1; i >= 0; i--) {
    const info = messages[i].info;
    if (!info) continue;
    if (info.role && info.role !== "assistant") continue;
    if (info.agent === "compaction") continue;
    if (info.agent) {
      const providerID = info.providerID ?? info.model?.providerID;
      const modelID = info.modelID ?? info.model?.modelID;
      return {
        agent: info.agent,
        model: providerID && modelID ? { providerID, modelID } : undefined,
      };
    }
  }
  return undefined;
}

async function resolveAgentForSession(
  ctx: PluginInput,
  sessionID: string,
  warningLabel: string
): Promise<ResolvedAgent | undefined> {
  try {
    const messagesResp = await ctx.client.session.messages({
      path: { id: sessionID },
      query: { directory: ctx.directory },
    });
    const messages = (
      isRecord(messagesResp) && Array.isArray(messagesResp.data) ? messagesResp.data : []
    ) as SessionMessage[];
    return resolveAgentFromMessages(messages);
  } catch (err) {
    console.warn(`[opencode-legion] ${warningLabel}`, err);
    return undefined;
  }
}

export interface TodoContinuationEnforcerOptions {
  isContinuationStopped: (sessionID: string) => boolean;
  isBackgroundSession: (sessionID: string) => boolean;
  isRecovering: (sessionID: string) => boolean;
  gracePeriodMs?: number;
}

export interface TodoContinuationEnforcer {
  event: (input: SessionEventInput) => Promise<void>;
  chatMessage: (input: ChatMessageInput) => Promise<void>;
  cancelPending: (sessionID: string) => void;
}

export function createTodoContinuationEnforcerHook(
  ctx: PluginInput,
  options: TodoContinuationEnforcerOptions
): TodoContinuationEnforcer {
  const pendingTimers = new Map<string, ReturnType<typeof setTimeout>>();
  /** Generation counter per session — incremented on any invalidation event. */
  const generations = new Map<string, number>();
  const gracePeriodMs = options.gracePeriodMs ?? DEFAULT_GRACE_PERIOD_MS;

  const getGeneration = (sessionID: string): number => {
    return generations.get(sessionID) ?? 0;
  };

  const bumpGeneration = (sessionID: string): void => {
    generations.set(sessionID, getGeneration(sessionID) + 1);
  };

  const cancelPending = (sessionID: string): void => {
    bumpGeneration(sessionID);
    const existing = pendingTimers.get(sessionID);
    if (existing) {
      clearTimeout(existing);
      pendingTimers.delete(sessionID);
    }
  };

  const handleIdle = async (sessionID: string): Promise<void> => {
    if (options.isContinuationStopped(sessionID)) return;
    if (options.isBackgroundSession(sessionID)) return;
    if (options.isRecovering(sessionID)) return;

    // Capture generation before async work — if it changes during awaits,
    // the timer will detect staleness and bail.
    const startGeneration = getGeneration(sessionID);

    let todos: TodoItem[];
    try {
      const response = await ctx.client.session.todo({ path: { id: sessionID } });
      todos = extractTodos(response);
    } catch (err) {
      console.warn("[opencode-legion] Failed to fetch todos for continuation:", err);
      return;
    }

    if (todos.length === 0 || getIncompleteCount(todos) === 0) return;

    // C2: Check generation after first await — bail if invalidated
    if (getGeneration(sessionID) !== startGeneration) return;

    const resolved = await resolveAgentForSession(
      ctx,
      sessionID,
      "Failed to fetch messages for continuation:"
    );

    if (!resolved) return;
    if (!CONTINUATION_AGENTS.has(resolved.agent)) return;

    // C2: Check generation after second await
    if (getGeneration(sessionID) !== startGeneration) return;

    cancelPending(sessionID);

    // Capture the generation AFTER cancelPending (which bumps it)
    const timerGeneration = getGeneration(sessionID);

    const timer = setTimeout(async () => {
      pendingTimers.delete(sessionID);

      // C1: Re-check stop state when timer fires
      if (options.isContinuationStopped(sessionID)) return;
      if (options.isBackgroundSession(sessionID)) return;
      if (options.isRecovering(sessionID)) return;

      // C2: Bail if generation changed during grace period
      if (getGeneration(sessionID) !== timerGeneration) return;

      try {
        const freshResp = await ctx.client.session.todo({ path: { id: sessionID } });
        const freshTodos = extractTodos(freshResp);
        if (getIncompleteCount(freshTodos) === 0) return;
      } catch (err) {
        console.warn("[opencode-legion] Failed to re-check todos before continuation:", err);
        return;
      }

      // Re-resolve agent at fire time so we use the current agent, not the stale one (M4)
      const freshResolved = await resolveAgentForSession(
        ctx,
        sessionID,
        "Failed to re-resolve agent for continuation:"
      );

      if (!freshResolved || !CONTINUATION_AGENTS.has(freshResolved.agent)) return;

      try {
        await ctx.client.session.promptAsync({
          path: { id: sessionID },
          body: {
            agent: freshResolved.agent,
            ...(freshResolved.model ? { model: freshResolved.model } : {}),
            parts: [{ type: "text" as const, text: CONTINUATION_PROMPT }],
          },
          query: { directory: ctx.directory },
        });
      } catch (err) {
        console.warn("[opencode-legion] Failed to inject continuation prompt:", err);
      }
    }, gracePeriodMs);

    pendingTimers.set(sessionID, timer);
  };

  const event = async ({ event }: SessionEventInput): Promise<void> => {
    const props = isRecord(event.properties) ? event.properties : undefined;

    if (event.type === "session.idle") {
      const sessionID = resolveSessionID(props);
      if (sessionID) {
        await handleIdle(sessionID);
      }
      return;
    }

    if (event.type === "session.deleted") {
      // C3: Use resolveSessionID which handles both props.sessionID and props.info.id
      const sessionID = resolveSessionID(props);
      if (sessionID) {
        cancelPending(sessionID);
        generations.delete(sessionID);
      }
    }
  };

  const chatMessage = async ({ sessionID }: ChatMessageInput): Promise<void> => {
    if (sessionID) {
      cancelPending(sessionID);
    }
  };

  return { event, chatMessage, cancelPending };
}
