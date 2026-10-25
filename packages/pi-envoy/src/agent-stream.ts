import {
  AGENT_STREAM_LIMITS,
  AGENT_STREAM_PROTOCOL,
  AGENT_STREAM_WATCH_TTL_MS,
  type AgentStreamCommand,
  type AgentStreamFrame,
  type AgentStreamPart,
  type AgentStreamReplay,
  type AgentStreamSettings,
  type AgentStreamSettingsReported,
  type AgentStreamSettingsUnreported,
  capAgentStreamText,
  isTypableCommandName,
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
  readonly model?: unknown;
  readonly provider?: unknown;
  readonly tag?: unknown;
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
  /** `provider/model` combined (e.g. `anthropic/claude-opus-5`), when the host message named
   *  both as strings; undefined for a user message, a tool result, and an assistant message
   *  whose host object names neither (an adapter with no access to its own model identity). */
  readonly model: string | undefined;
  /** The tag the host kept on a typed input's user message (its Dispatch message id). */
  readonly tag: string | undefined;
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

/** What a `settings` frame says without its envelope: what the session's host reports of its
 *  model and thinking level, or that it cannot report them. */
export type AgentStreamSettingsState =
  | Omit<AgentStreamSettingsReported, "v" | "kind" | "seq">
  | Omit<AgentStreamSettingsUnreported, "v" | "kind" | "seq">;

const encoder = new TextEncoder();

/** What a replay's `frames` array costs beyond its frames: its two brackets. Each frame after the
 *  first adds a comma. */
const REPLAY_ARRAY_BYTES = 2;

/** The UTF-8 bytes `value` serialises to, which is what NATS measures. */
function jsonBytes(value: unknown): number {
  return encoder.encode(JSON.stringify(value)).length;
}

/** `provider/id` combined (e.g. `anthropic/claude-opus-5`), or undefined when either is missing or
 *  empty — an empty string still satisfies `typeof x === "string"`, and combining one anyway would
 *  publish a malformed `/claude-opus-5` or `anthropic/`. Never capped: a settings frame names the
 *  model a person's pick sends back, so it must be the exact id. */
export function modelReference(provider: unknown, id: unknown): string | undefined {
  if (typeof id !== "string" || id === "") return undefined;
  if (typeof provider !== "string" || provider === "") return undefined;
  return `${provider}/${id}`;
}

/** The model an assistant message names, `provider/model` combined and capped. */
function readModel(message: HostMessage): string | undefined {
  const model = modelReference(message.provider, message.model);
  return model === undefined
    ? undefined
    : capAgentStreamText(model, AGENT_STREAM_LIMITS.modelChars);
}

function readMessage(raw: unknown): ReadableMessage | null {
  if (typeof raw !== "object" || raw === null) return null;
  const message = raw as HostMessage;
  // A message with no host timestamp has no identity that survives its own stream, and the
  // viewer orders the conversation by it. The host stamps every message it keeps.
  if (typeof message.role !== "string" || typeof message.timestamp !== "number") return null;
  return {
    host: message,
    isError: message.isError === true,
    model: readModel(message),
    role: message.role,
    timestamp: message.timestamp,
    toolCallId: typeof message.toolCallId === "string" ? message.toolCallId : undefined,
    toolName: typeof message.toolName === "string" ? message.toolName : undefined,
    tag: typeof message.tag === "string" ? message.tag : undefined,
  };
}

/** The ring slot a message occupies, or null for a message this build does not carry. A tagged
 *  user message's slot names its tag too: two Sends can be stamped in the same millisecond. */
function keyFor(message: ReadableMessage): string | null {
  if (message.role === "toolResult") {
    return message.toolCallId === undefined ? null : `t:${message.toolCallId}`;
  }
  if (message.role === "assistant") return `m:a${message.timestamp}`;
  if (message.role !== "user") return null;
  return message.tag === undefined
    ? `m:u${message.timestamp}`
    : `m:u${message.timestamp}:${message.tag}`;
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
      // Only an assistant turn ever carries one: the host's own UserMessage has no model or
      // provider field to read, but a forged frame on the bus could still claim one, and the
      // contract promises "absent on a user message".
      ...(message.role === "assistant" && message.model !== undefined
        ? { model: message.model }
        : {}),
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
  /** The slash commands the session takes from Dispatch, capped and each name once, before the
   *  byte fit; undefined for a session whose host cannot run typed input, which publishes none. */
  #listedCommands: readonly AgentStreamCommand[] | undefined;
  /** The host-order prefix of `#listedCommands` that fits the replay, as last published. */
  #commands: readonly AgentStreamCommand[] | undefined;
  #commandsSeq = 0;
  /** The session's model and thinking level, as last published; undefined until first read. */
  #settings: AgentStreamSettingsState | undefined;
  #settingsSeq = 0;
  /** `#settings` as JSON, which a new state is compared against. */
  #settingsJson: string | undefined;

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
    this.#listedCommands = undefined;
    this.#commands = undefined;
    this.#settings = undefined;
    this.#settingsJson = undefined;
  }

  /**
   * The session's slash commands: kept for every replay, whose last frame they are, and published
   * at once when they differ from the list last kept while a viewer is attached. Each name is
   * capped and kept once, a name a person could not type dropped (`isTypableCommandName`). The
   * list keeps the host-order prefix whose frame fits what the settings frame leaves of the
   * replay's byte budget (`#fitCommands`): the caps count characters, and a character JSON escapes
   * (`\u0001`) costs six bytes.
   */
  setCommands(subject: string, commands: readonly AgentStreamCommand[]): void {
    const seen = new Set<string>();
    const listed: AgentStreamCommand[] = [];
    for (const command of commands) {
      if (listed.length >= AGENT_STREAM_LIMITS.commands) break;
      const name = command.name;
      if (name.length > AGENT_STREAM_LIMITS.commandNameChars) continue;
      if (!isTypableCommandName(name) || seen.has(name)) continue;
      seen.add(name);
      listed.push({
        name,
        source: command.source,
        ...(command.description !== undefined && {
          description: capAgentStreamText(
            command.description,
            AGENT_STREAM_LIMITS.commandDescriptionChars
          ),
        }),
        ...(command.terminalOnly === true && { terminalOnly: true as const }),
      });
    }
    this.#listedCommands = listed;
    this.#fitCommands(subject);
  }

  /**
   * The session's model and thinking level: kept for every replay, whose frame before the commands
   * it is, and published at once when they differ from what was last kept while a viewer is
   * attached. Never cut: a replay pays for this frame first, whole, and the commands then keep the
   * prefix that fits in what is left, so a change here can change the commands frame too.
   */
  setSettings(subject: string, settings: AgentStreamSettingsState): void {
    const kept: AgentStreamSettingsState = settings.reported
      ? {
          ...(settings.model !== undefined && { model: settings.model }),
          models: [...settings.models],
          reported: true,
          thinking:
            settings.thinking === null
              ? null
              : { level: settings.thinking.level, levels: [...settings.thinking.levels] },
        }
      : { reported: false };
    const json = JSON.stringify(kept);
    if (json === this.#settingsJson) return;
    this.#settings = kept;
    this.#settingsJson = json;
    this.#seq += 1;
    this.#settingsSeq = this.#seq;
    if (this.watched) this.#deps.publish(subject, JSON.stringify(this.#settingsFrame(kept)));
    this.#fitCommands(subject);
  }

  /** Keeps the host-order prefix of the listed commands whose frame fits the replay's byte budget
   *  after the settings frame, and publishes it when it differs from the list last kept. */
  #fitCommands(subject: string): void {
    const listed = this.#listedCommands;
    if (listed === undefined) return;
    // Measured as `replay` pays for it: the whole settings frame and the comma after it, then this
    // frame under the highest sequence number it can carry, then each command and the comma
    // before it.
    const settings =
      this.#settings === undefined ? 0 : jsonBytes(this.#settingsFrame(this.#settings)) + 1;
    const limit = AGENT_STREAM_LIMITS.historyBytes - REPLAY_ARRAY_BYTES - settings;
    let bytes = jsonBytes(this.#commandsFrame([], this.#seq + 1));
    const kept: AgentStreamCommand[] = [];
    for (const command of listed) {
      const cost = jsonBytes(command) + (kept.length === 0 ? 0 : 1);
      if (bytes + cost > limit) break;
      bytes += cost;
      kept.push(command);
    }
    if (JSON.stringify(kept) === JSON.stringify(this.#commands)) return;
    this.#commands = kept;
    this.#seq += 1;
    this.#commandsSeq = this.#seq;
    if (this.watched) this.#deps.publish(subject, JSON.stringify(this.#commandsFrame(kept)));
  }

  #commandsFrame(
    commands: readonly AgentStreamCommand[],
    seq = this.#commandsSeq
  ): AgentStreamFrame {
    return { commands, kind: "commands", seq, v: AGENT_STREAM_PROTOCOL };
  }

  #settingsFrame(settings: AgentStreamSettingsState): AgentStreamSettings {
    return { ...settings, kind: "settings", seq: this.#settingsSeq, v: AGENT_STREAM_PROTOCOL };
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
   * over the server's 1 MiB `max_payload`, and the publish would throw. It bounds what `frames`
   * serialises to: every frame, the commas between them and the array's brackets.
   */
  replay(sessionID: string): AgentStreamReplay {
    if (!this.watched) return { frames: [], session_id: sessionID, v: AGENT_STREAM_PROTOCOL };
    const ordered = [...this.#history.values()].sort((left, right) =>
      left.at === right.at ? left.seq - right.seq : left.at - right.at
    );
    // The settings and then the command list ride last, in that order, and are paid for first, so
    // a full history never crowds them out; `#fitCommands` keeps the two inside the budget.
    const tail: AgentStreamFrame[] = [];
    if (this.#settings !== undefined) tail.push(this.#settingsFrame(this.#settings));
    if (this.#commands !== undefined) tail.push(this.#commandsFrame(this.#commands));
    let used =
      REPLAY_ARRAY_BYTES +
      tail.reduce((bytes, frame, index) => bytes + jsonBytes(frame) + (index === 0 ? 0 : 1), 0);
    const kept: AgentStreamFrame[] = [];
    for (let index = ordered.length - 1; index >= 0; index -= 1) {
      const entry = ordered[index];
      if (entry === undefined) continue;
      const frame = frameFor(entry);
      const separated = kept.length > 0 || tail.length > 0;
      const cost = jsonBytes(frame) + (separated ? 1 : 0);
      if (used + cost > AGENT_STREAM_LIMITS.historyBytes) break;
      used += cost;
      kept.unshift(frame);
    }
    kept.push(...tail);
    return { frames: kept, session_id: sessionID, v: AGENT_STREAM_PROTOCOL };
  }
}
