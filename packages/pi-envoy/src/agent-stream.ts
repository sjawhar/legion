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
 * The ring holds the host's own message object, not a built frame. A streamed tool call arrives
 * as one `message_update` per delta — tens of thousands for a large write — and building a frame
 * for each would serialise the whole growing argument object every time, which is quadratic in
 * its size and was measured at 1.2 s of main-thread CPU for a 100 KB call in a session nobody
 * was even watching. Frames are built where they are actually needed: at a publish the throttle
 * allows, and at a replay.
 *
 * Ordering never depends on the host's delivery order, because there isn't one to depend on.
 * Oh My Pi 18.3.2 hands `message_update` to extensions through its own queue while
 * `message_start` and `message_end` are emitted directly, so a message's last few updates arrive
 * *after* it has already settled. A settled message is therefore final here: once a key holds
 * one, a later streaming record for it is dropped outright — no ring write, no sequence number —
 * so a finished turn can never be left looking like a running one.
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

/**
 * One message, with the identity and role a frame needs, once the host's shape checks out. The
 * host's own object is kept rather than its content: reading `content` is what costs, and a
 * streaming message's is replaced by the host on every delta, so the reference is always the
 * latest without this ever copying one.
 */
interface ReadableMessage {
  readonly role: string;
  readonly timestamp: number;
  readonly toolCallId: string | undefined;
  readonly toolName: string | undefined;
  readonly isError: boolean;
  readonly host: HostMessage;
}

/** One message or tool result the session may still replay. */
interface RingEntry {
  readonly key: string;
  readonly at: number;
  readonly seq: number;
  /** False once the host has settled this message; a settled entry is never replaced. */
  readonly streaming: boolean;
  readonly message: ReadableMessage;
  /** The Dispatch message a user message delivered, when the caller says it is one: a person's
   *  direct message the session took as its own user turn. */
  readonly dispatchMessageId: string | undefined;
}

export interface AgentStreamPublisherDeps {
  /** A core NATS publish. Never JetStream: see the subject note in `@legion/contracts`. */
  readonly publish: (subject: string, payload: string) => void;
  readonly now: () => number;
}

const encoder = new TextEncoder();

function readMessage(raw: unknown): ReadableMessage | null {
  if (typeof raw !== "object" || raw === null) return null;
  const message = raw as HostMessage;
  // A message with no host timestamp has no identity that survives its own stream, and the
  // viewer orders the conversation by it. The host stamps every message it keeps.
  if (typeof message.role !== "string" || typeof message.timestamp !== "number") return null;
  return {
    host: message,
    isError: message.isError === true,
    role: message.role,
    timestamp: message.timestamp,
    toolCallId: typeof message.toolCallId === "string" ? message.toolCallId : undefined,
    toolName: typeof message.toolName === "string" ? message.toolName : undefined,
  };
}

/** The ring slot a message occupies, or null for a message this build does not carry. */
function keyFor(message: ReadableMessage): string | null {
  if (message.role === "toolResult") {
    return message.toolCallId === undefined ? null : `t:${message.toolCallId}`;
  }
  if (message.role !== "assistant" && message.role !== "user") return null;
  return `m:${message.role === "assistant" ? "a" : "u"}${message.timestamp}`;
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

/** Caps every string a tool call's arguments carry as they are serialised, so the cost of
 *  rendering a call that streams a large file in is bounded by the cap rather than by the file. */
function boundedStrings(_key: string, value: unknown): unknown {
  return typeof value === "string"
    ? capAgentStreamText(value, AGENT_STREAM_LIMITS.toolChars)
    : value;
}

/** A tool call's arguments as the viewer shows them. They stream in, so the value is often a
 *  partial object and must survive being unserialisable. */
function renderArguments(value: unknown): string {
  if (value === undefined || value === null) return "";
  try {
    return JSON.stringify(value, boundedStrings, 2) ?? "";
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

/** The frame a ring entry becomes. This is the only place a message's content is read. */
function frameFor(entry: RingEntry): AgentStreamFrame {
  const { dispatchMessageId, message } = entry;
  if (message.role === "toolResult" && message.toolCallId !== undefined) {
    return {
      kind: "tool-result",
      result: {
        at: message.timestamp,
        isError: message.isError,
        output: capAgentStreamText(
          contentText(message.host.content),
          AGENT_STREAM_LIMITS.toolChars
        ),
        toolCallId: message.toolCallId,
        toolName: message.toolName ?? "tool",
      },
      seq: entry.seq,
      v: AGENT_STREAM_PROTOCOL,
    };
  }
  return {
    kind: "message",
    message: {
      at: message.timestamp,
      id: entry.key.slice(2),
      parts: conversationParts(message.host.content),
      role: message.role === "assistant" ? "assistant" : "user",
      streaming: entry.streaming,
      ...(dispatchMessageId === undefined ? {} : { dispatchMessageId }),
    },
    seq: entry.seq,
    v: AGENT_STREAM_PROTOCOL,
  };
}

export class AgentStreamPublisher {
  readonly #deps: AgentStreamPublisherDeps;
  /** Newest state per message or tool result, in the order each was first seen. */
  readonly #history = new Map<string, RingEntry>();
  /** Keys the host has settled. A settled message is final; see the class note. */
  readonly #settled = new Set<string>();
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
   * Forget everything: the session under this publisher has been replaced. `/new`, `/resume`, a
   * fork and a tree navigation all move the session id while the extension instance lives on,
   * and a viewer opening the new id must not be served the previous conversation.
   */
  reset(): void {
    this.#history.clear();
    this.#settled.clear();
    this.#publishedAt.clear();
    this.#watchedUntil = 0;
  }

  /**
   * Records one of the session's messages and, while a viewer is attached, publishes it.
   * `streaming` is false for a settled message (`message_end`, and every user or tool-result
   * message) and true for an assistant message still being produced. `dispatchMessageId` is the
   * Dispatch message a user message delivered, which the caller alone can know; a later record of
   * the same message keeps it. Returns whether the message was recorded: false for one this build
   * does not carry, and for a late streaming update of a message the host has already settled.
   */
  record(subject: string, raw: unknown, streaming: boolean, dispatchMessageId?: string): boolean {
    const message = readMessage(raw);
    if (message === null) return false;
    const key = keyFor(message);
    if (key === null) return false;
    const stillStreaming = message.role === "assistant" && streaming;
    // The host's updates outlive its own `message_end`, so a settled message is final.
    if (stillStreaming && this.#settled.has(key)) return false;
    this.#seq += 1;
    const entry: RingEntry = {
      at: message.timestamp,
      dispatchMessageId: dispatchMessageId ?? this.#history.get(key)?.dispatchMessageId,
      key,
      message,
      seq: this.#seq,
      streaming: stillStreaming,
    };
    this.#history.set(key, entry);
    if (!stillStreaming) this.#settled.add(key);
    while (this.#history.size > AGENT_STREAM_LIMITS.historyMessages) {
      const oldest = this.#history.keys().next();
      if (oldest.done === true) break;
      this.#history.delete(oldest.value);
      this.#settled.delete(oldest.value);
      this.#publishedAt.delete(oldest.value);
    }
    if (!this.watched) return true;
    const now = this.#deps.now();
    const published = this.#publishedAt.get(key);
    const tooSoon =
      published !== undefined && now - published < AGENT_STREAM_LIMITS.snapshotIntervalMs;
    if (stillStreaming && tooSoon) return true;
    this.#publishedAt.set(key, now);
    this.#deps.publish(subject, JSON.stringify(frameFor(entry)));
    return true;
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
   *
   * The budget is UTF-8 bytes, which is what NATS measures. Counting UTF-16 code units would let
   * a history of box-drawing tool output or CJK text build a reply three times its measured size,
   * over the server's 1 MiB `max_payload`, and the publish would throw.
   */
  replay(sessionID: string): AgentStreamReplay {
    if (!this.watched) return { frames: [], session_id: sessionID, v: AGENT_STREAM_PROTOCOL };
    const ordered = [...this.#history.values()].sort((left, right) =>
      left.at === right.at ? left.seq - right.seq : left.at - right.at
    );
    let budget = AGENT_STREAM_LIMITS.historyBytes;
    const kept: AgentStreamFrame[] = [];
    for (let index = ordered.length - 1; index >= 0; index -= 1) {
      const entry = ordered[index];
      if (entry === undefined) continue;
      const frame = frameFor(entry);
      budget -= encoder.encode(JSON.stringify(frame)).length;
      if (budget < 0) break;
      kept.unshift(frame);
    }
    return { frames: kept, session_id: sessionID, v: AGENT_STREAM_PROTOCOL };
  }
}
