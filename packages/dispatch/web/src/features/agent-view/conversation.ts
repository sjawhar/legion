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

/**
 * Whether a frame is one this build can render. Frames arrive as JSON off a bus any client can
 * publish on, and from a publisher that ships separately from this dashboard, so a shape this
 * code does not expect is a live possibility rather than a theoretical one. assistant-ui throws
 * on a tool-call or reasoning part attached to a user message and on any unknown role, and that
 * throw takes the whole page to the route's error screen — on every load, because the session's
 * ring serves the same frame again. A frame that does not check out is dropped instead.
 */
export function isRenderableFrame(frame: AgentStreamFrame): boolean {
  if (frame.v !== 1 || typeof frame.seq !== "number") return false;
  if (frame.kind === "tool-result") {
    const { result } = frame;
    return (
      typeof result?.toolCallId === "string" &&
      typeof result.toolName === "string" &&
      typeof result.output === "string" &&
      typeof result.at === "number"
    );
  }
  if (frame.kind !== "message") return false;
  const { message } = frame;
  if (message === undefined || message === null) return false;
  if (typeof message.id !== "string" || typeof message.at !== "number") return false;
  if (message.role !== "user" && message.role !== "assistant") return false;
  if (!Array.isArray(message.parts)) return false;
  return message.parts.every((part) => {
    if (part.type === "text") return typeof part.text === "string";
    // Text is the only part a user message may carry. assistant-ui throws
    // "Unsupported user message part type: reasoning" on the others, exactly as it does for a
    // tool call, and the throw takes the thread down with it.
    if (part.type === "reasoning") {
      return message.role === "assistant" && typeof part.text === "string";
    }
    if (part.type !== "tool-call") return false;
    return (
      message.role === "assistant" &&
      typeof part.toolCallId === "string" &&
      typeof part.toolName === "string" &&
      typeof part.argsText === "string"
    );
  });
}

export function applyFrame(state: AgentConversation, frame: AgentStreamFrame): AgentConversation {
  if (!isRenderableFrame(frame)) return state;
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
  // A settled message is final, whatever sequence number a later snapshot carries. The host
  // hands the extension a message's last few `message_update`s after its `message_end`, so the
  // late streaming snapshot is the one with the higher number; letting it win leaves a finished
  // turn rendered as running, and assistant-ui then disables the composer.
  const settled = state.messages.find(
    (message) => message.id === frame.message.id && !message.streaming
  );
  if (settled !== undefined && frame.message.streaming) {
    return { ...state, applied: { ...state.applied, [key]: frame.seq } };
  }
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

/** One content part of a message assistant-ui renders. */
type ThreadPart = Exclude<ThreadMessageLike["content"], string>[number];

/**
 * The conversation as assistant-ui renders it: a tool call carries its own result.
 *
 * A user message emits text and nothing else. assistant-ui throws on any other part type there
 * ("Unsupported user message part type: reasoning"), and that throw happens where the runtime is
 * built — above the thread, so no boundary around the thread can catch it and the composer goes
 * with the transcript. `isRenderableFrame` already drops such a frame; this is the second guard,
 * because a part type a future publisher adds would otherwise reach the library through a frame
 * this build did not know to refuse.
 */
export function toThreadMessages(state: AgentConversation): ThreadMessageLike[] {
  return state.messages.map((message) => ({
    content: message.parts.flatMap<ThreadPart>((part) => {
      if (part.type === "text") return [{ text: part.text, type: "text" as const }];
      if (message.role !== "assistant") return [];
      if (part.type === "reasoning") return [{ text: part.text, type: "reasoning" as const }];
      const result = state.results[part.toolCallId];
      return [
        {
          argsText: part.argsText,
          isError: result?.isError ?? false,
          result: result?.output,
          toolCallId: part.toolCallId,
          toolName: part.toolName,
          type: "tool-call" as const,
        },
      ];
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
