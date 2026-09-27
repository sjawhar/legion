import type { ThreadMessageLike } from "@assistant-ui/react";
import type {
  AgentStreamFrame,
  AgentStreamMessage,
  AgentStreamToolResult,
} from "@legion/contracts";

/**
 * The live conversation a session is streaming, assembled from frames that arrive in whatever
 * order the session's own concurrent handlers produced them.
 *
 * Every frame is a whole snapshot of one message, so applying them is last-writer-wins by the
 * session's monotonic `seq`: a frame that lost a race is dropped rather than overwriting a newer
 * snapshot of the same message. Order in the thread is the host's own message timestamp, never
 * arrival order.
 */
export interface AgentConversation {
  readonly messages: readonly AgentStreamMessage[];
  /** Tool results keyed by tool call id, which is how they find their call. */
  readonly results: Readonly<Record<string, AgentStreamToolResult>>;
  /** The highest `seq` already applied per message id and per tool call id. */
  readonly applied: Readonly<Record<string, number>>;
}

export const EMPTY_CONVERSATION: AgentConversation = {
  applied: {},
  messages: [],
  results: {},
};

export function applyFrame(state: AgentConversation, frame: AgentStreamFrame): AgentConversation {
  if (frame.kind === "tool-result") {
    const key = `t:${frame.result.toolCallId}`;
    if ((state.applied[key] ?? 0) >= frame.seq) return state;
    return {
      applied: { ...state.applied, [key]: frame.seq },
      messages: state.messages,
      results: { ...state.results, [frame.result.toolCallId]: frame.result },
    };
  }
  const key = `m:${frame.message.id}`;
  if ((state.applied[key] ?? 0) >= frame.seq) return state;
  const others = state.messages.filter((message) => message.id !== frame.message.id);
  const messages = [...others, frame.message].sort((left, right) =>
    left.at === right.at ? left.id.localeCompare(right.id) : left.at - right.at
  );
  return {
    applied: { ...state.applied, [key]: frame.seq },
    messages,
    results: state.results,
  };
}

export function applyFrames(
  state: AgentConversation,
  frames: readonly AgentStreamFrame[]
): AgentConversation {
  return frames.reduce(applyFrame, state);
}

/** The conversation as assistant-ui renders it: a tool call carries its own result. */
export function toThreadMessages(state: AgentConversation): ThreadMessageLike[] {
  return state.messages.map((message) => ({
    content: message.parts.map((part) => {
      if (part.type === "text") return { text: part.text, type: "text" as const };
      if (part.type === "reasoning") return { text: part.text, type: "reasoning" as const };
      const result = state.results[part.toolCallId];
      return {
        argsText: part.argsText,
        isError: result?.isError ?? false,
        result: result?.output,
        toolCallId: part.toolCallId,
        toolName: part.toolName,
        type: "tool-call" as const,
      };
    }),
    createdAt: new Date(message.at),
    id: message.id,
    role: message.role,
    // A status belongs to an assistant message alone: assistant-ui refuses one on a user
    // message, and a whole page would fail to render rather than one turn.
    ...(message.role === "assistant"
      ? {
          status: message.streaming
            ? ({ type: "running" } as const)
            : ({ reason: "stop", type: "complete" } as const),
        }
      : {}),
  }));
}

/** Whether the session is mid-turn, which is what shows the composer a running thread. */
export function isRunning(state: AgentConversation): boolean {
  return state.messages.some((message) => message.streaming);
}
