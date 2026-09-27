import {
  AGENT_STREAM_LIMITS,
  AGENT_STREAM_PROTOCOL,
  AGENT_STREAM_WATCH_TTL_MS,
  type AgentStreamFrame,
  type AgentStreamPart,
  type AgentStreamReplay,
  capAgentStreamText,
} from "@legion/contracts";

/**
 * Turns this Oh My Pi session's own messages into the frames a Dispatch viewer renders
 * (LEGION-232).
 *
 * Two rules shape it. **Nothing leaves the session unwatched:** a frame is published only inside
 * the window a viewer's `watch` or `replay` arms, so a session nobody is looking at puts no
 * conversation on the bus at all. **Nothing is stored anywhere else:** the only history is the
 * bounded ring below, in this process, beside the transcript the session already keeps on disk;
 * the relay and the dashboard hold frames no longer than a viewer's connection.
 *
 * The host emits `message_start`, `message_update` and `message_end` concurrently — measured on
 * omp 18.3.2, a message's `message_end` can reach a handler before its `message_start` — so this
 * publisher never depends on their order. Every frame carries the host's own message timestamp
 * and a monotonic `seq`, and a viewer keyed by message id applies whichever frame has the higher
 * `seq`.
 */

/**
 * A message and its content parts as an Oh My Pi `message_*` handler receives them. Every field
 * a frame reads is `unknown`: the host's own types are not importable here, and a build that
 * renames one must be caught by the checks below rather than by a cast that assumes it.
 */
interface HostMessage {
  readonly role?: unknown;
  readonly content?: unknown;
  readonly timestamp?: unknown;
  readonly toolCallId?: unknown;
  readonly toolName?: unknown;
  readonly isError?: unknown;
}

interface HostPart {
  readonly type?: unknown;
  readonly text?: unknown;
  readonly thinking?: unknown;
  readonly id?: unknown;
  readonly name?: unknown;
  readonly arguments?: unknown;
}

/** One message, with the identity and role a frame needs, once the host's shape checks out. */
interface ReadableMessage {
  readonly role: string;
  readonly content: unknown;
  readonly timestamp: number;
  readonly toolCallId: string | undefined;
  readonly toolName: string | undefined;
  readonly isError: boolean;
}

export interface AgentStreamPublisherDeps {
  /** A core NATS publish. Never JetStream: see the subject note in `@legion/contracts`. */
  readonly publish: (subject: string, payload: string) => void;
  readonly now: () => number;
}

function readMessage(raw: unknown): ReadableMessage | null {
  if (typeof raw !== "object" || raw === null) return null;
  const message = raw as HostMessage;
  // A message with no host timestamp has no identity that survives its own stream, and the
  // viewer orders the conversation by it. The host stamps every message it keeps.
  if (typeof message.role !== "string" || typeof message.timestamp !== "number") return null;
  return {
    content: message.content,
    isError: message.isError === true,
    role: message.role,
    timestamp: message.timestamp,
    toolCallId: typeof message.toolCallId === "string" ? message.toolCallId : undefined,
    toolName: typeof message.toolName === "string" ? message.toolName : undefined,
  };
}

/** Flattens a message's content parts to plain text, which is how a tool result reads. */
function contentText(content: unknown): string {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  const pieces: string[] = [];
  for (const raw of content) {
    if (typeof raw !== "object" || raw === null) continue;
    const part = raw as HostPart;
    if (typeof part.text === "string") pieces.push(part.text);
    else if (typeof part.type === "string") pieces.push(`[${part.type}]`);
  }
  return pieces.join("\n");
}

/** A tool call's arguments as the viewer shows them. They stream in, so the value is often a
 *  partial object and must survive being unserialisable. */
function renderArguments(value: unknown): string {
  if (value === undefined || value === null) return "";
  try {
    return JSON.stringify(value, null, 2) ?? "";
  } catch {
    return "[unserialisable arguments]";
  }
}

function conversationParts(content: unknown): AgentStreamPart[] {
  const { partChars, toolChars } = AGENT_STREAM_LIMITS;
  if (typeof content === "string") {
    return content === "" ? [] : [{ text: capAgentStreamText(content, partChars), type: "text" }];
  }
  if (!Array.isArray(content)) return [];
  const parts: AgentStreamPart[] = [];
  for (const raw of content) {
    if (typeof raw !== "object" || raw === null) continue;
    const part = raw as HostPart;
    if (part.type === "text" && typeof part.text === "string") {
      parts.push({ text: capAgentStreamText(part.text, partChars), type: "text" });
    } else if (part.type === "thinking" && typeof part.thinking === "string") {
      parts.push({ text: capAgentStreamText(part.thinking, partChars), type: "reasoning" });
    } else if (part.type === "toolCall" && typeof part.id === "string") {
      parts.push({
        argsText: capAgentStreamText(renderArguments(part.arguments), toolChars),
        toolCallId: part.id,
        toolName: typeof part.name === "string" ? part.name : "tool",
        type: "tool-call",
      });
    } else if (typeof part.type === "string") {
      // An image, or any part kind this build does not know: named, never dropped silently,
      // and never carrying its bytes.
      parts.push({ text: `[${part.type}]`, type: "text" });
    }
  }
  return parts;
}

export class AgentStreamPublisher {
  readonly #deps: AgentStreamPublisherDeps;
  /** Newest snapshot per message or tool result, in the order each was first seen. */
  readonly #history = new Map<string, AgentStreamFrame>();
  /** When each message was last put on the wire, so a token storm costs one frame per
   *  `snapshotIntervalMs` rather than one per delta. */
  readonly #publishedAt = new Map<string, number>();
  #seq = 0;
  #watchedUntil = 0;

  constructor(deps: AgentStreamPublisherDeps) {
    this.#deps = deps;
  }

  /** A viewer asked for this session: publish for the next `AGENT_STREAM_WATCH_TTL_MS`. */
  noteViewer(): void {
    this.#watchedUntil = this.#deps.now() + AGENT_STREAM_WATCH_TTL_MS;
  }

  get watched(): boolean {
    return this.#deps.now() < this.#watchedUntil;
  }

  /**
   * Records one of the session's messages and, while a viewer is attached, publishes it.
   * `streaming` is false for a settled message (`message_end`, and every user or tool-result
   * message) and true for an assistant message still being produced. Returns the frame it
   * recorded, or null for a message this build does not put on the wire.
   */
  record(subject: string, raw: unknown, streaming: boolean): AgentStreamFrame | null {
    const message = readMessage(raw);
    if (message === null) return null;
    const frame = this.#frameFor(message, streaming);
    if (frame === null) return null;
    const key = frame.kind === "message" ? `m:${frame.message.id}` : `t:${frame.result.toolCallId}`;
    this.#history.set(key, frame);
    while (this.#history.size > AGENT_STREAM_LIMITS.historyMessages) {
      const oldest = this.#history.keys().next();
      if (oldest.done === true) break;
      this.#history.delete(oldest.value);
      this.#publishedAt.delete(oldest.value);
    }
    if (!this.watched) return frame;
    const now = this.#deps.now();
    const published = this.#publishedAt.get(key);
    const tooSoon =
      published !== undefined && now - published < AGENT_STREAM_LIMITS.snapshotIntervalMs;
    if (streaming && tooSoon) return frame;
    this.#publishedAt.set(key, now);
    this.#deps.publish(subject, JSON.stringify(frame));
    return frame;
  }

  /**
   * What this session can still replay, oldest first, inside the byte budget.
   *
   * Answered only while a viewer has already armed the session. The ring itself fills whether
   * or not anyone is watching — it never leaves this process, and it is what lets the first
   * viewer to open a session see the turn it is already in — but handing it out is publishing,
   * so it obeys the same rule as a frame: a session nobody has opened answers nothing. The
   * relay arms the session before it asks (`Watch` then `Replay` on one connection, in that
   * order), so a genuine viewer is never refused.
   */
  replay(sessionID: string): AgentStreamReplay {
    if (!this.watched) return { frames: [], session_id: sessionID, v: AGENT_STREAM_PROTOCOL };
    const ordered = [...this.#history.values()].sort((left, right) => {
      const leftAt = left.kind === "message" ? left.message.at : left.result.at;
      const rightAt = right.kind === "message" ? right.message.at : right.result.at;
      return leftAt === rightAt ? left.seq - right.seq : leftAt - rightAt;
    });
    let budget = AGENT_STREAM_LIMITS.historyBytes;
    const kept: AgentStreamFrame[] = [];
    for (let index = ordered.length - 1; index >= 0; index -= 1) {
      const frame = ordered[index];
      if (frame === undefined) continue;
      budget -= JSON.stringify(frame).length;
      if (budget < 0) break;
      kept.unshift(frame);
    }
    return { frames: kept, session_id: sessionID, v: AGENT_STREAM_PROTOCOL };
  }

  #frameFor(message: ReadableMessage, streaming: boolean): AgentStreamFrame | null {
    if (message.role === "toolResult") {
      if (message.toolCallId === undefined) return null;
      this.#seq += 1;
      return {
        kind: "tool-result",
        result: {
          at: message.timestamp,
          isError: message.isError,
          output: capAgentStreamText(contentText(message.content), AGENT_STREAM_LIMITS.toolChars),
          toolCallId: message.toolCallId,
          toolName: message.toolName ?? "tool",
        },
        seq: this.#seq,
        v: AGENT_STREAM_PROTOCOL,
      };
    }
    if (message.role !== "assistant" && message.role !== "user") return null;
    this.#seq += 1;
    return {
      kind: "message",
      message: {
        at: message.timestamp,
        id: `${message.role === "assistant" ? "a" : "u"}${message.timestamp}`,
        parts: conversationParts(message.content),
        role: message.role,
        streaming: message.role === "assistant" && streaming,
      },
      seq: this.#seq,
      v: AGENT_STREAM_PROTOCOL,
    };
  }
}
