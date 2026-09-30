/**
 * The live agent conversation stream: what an Oh My Pi session publishes about its own turns,
 * and what the Dispatch server relays to a browser watching it (LEGION-232).
 *
 * Nothing here is ever stored. The subjects deliberately sit outside `notifications.`, the only
 * prefix family the `ENVOY_NOTIFICATIONS` JetStream stream captures (`bus/stream.go`), so every
 * frame travels over core NATS and the bus retains none of it; the Dispatch server relays frames
 * to an attached viewer and writes no row and no log line carrying a frame body. A session's own
 * transcript on its host stays the record.
 *
 * The frame shape is assistant-ui's `ThreadMessageLike` reduced to what a session can produce, so
 * the dashboard's adapter is a rename rather than a translation and no harness knowledge reaches
 * the browser.
 */

/** Subject root of the stream. Outside `notifications.`: see the note above. */
export const AGENT_STREAM_SUBJECT_PREFIX = "agentstream." as const;

/** Wire version every frame and control message carries. */
export const AGENT_STREAM_PROTOCOL = 1 as const;

/** Where a session publishes its conversation frames. */
export function agentStreamFramesSubject(sessionID: string): string {
  return `${AGENT_STREAM_SUBJECT_PREFIX}${sessionID}.frames`;
}

/** Where a session listens for a viewer's replay request and watch pings. */
export function agentStreamControlSubject(sessionID: string): string {
  return `${AGENT_STREAM_SUBJECT_PREFIX}${sessionID}.control`;
}

/**
 * How long a `watch` or `replay` keeps a session publishing. A session with no viewer inside
 * this window puts nothing on the wire at all, which is why the relay pings while a viewer is
 * attached; it must therefore ping well inside the window.
 */
export const AGENT_STREAM_WATCH_TTL_MS = 30_000;

/** How often the relay re-arms a watched session. */
export const AGENT_STREAM_WATCH_INTERVAL_MS = 10_000;

/** Longest a viewer waits for the session's own replay before showing the live stream alone. */
export const AGENT_STREAM_REPLAY_TIMEOUT_MS = 2_000;

/**
 * Caps the publisher applies before a frame leaves the session. NATS refuses a payload over
 * 1 MiB, and a tool that reads a large file would otherwise re-send it on every snapshot.
 */
export const AGENT_STREAM_LIMITS = {
  /** Assistant or user prose, and reasoning, per part. */
  partChars: 16_000,
  /** A tool call's rendered arguments, and a tool result's output. */
  toolChars: 8_000,
  /** Messages the session keeps for replay. */
  historyMessages: 200,
  /** Bytes of replay the session will answer with, newest first. */
  historyBytes: 512 * 1024,
  /** Shortest gap between two snapshots of the same streaming message. */
  snapshotIntervalMs: 100,
} as const;

/** Marks text a cap cut short. */
export const AGENT_STREAM_TRUNCATION_SUFFIX = "\n… truncated";

export type AgentStreamPart =
  | { readonly type: "text"; readonly text: string }
  | { readonly type: "reasoning"; readonly text: string }
  | {
      readonly type: "tool-call";
      readonly toolCallId: string;
      readonly toolName: string;
      /** The call's arguments as pretty JSON; the viewer renders it, never re-parses it. */
      readonly argsText: string;
    };

export interface AgentStreamMessage {
  readonly id: string;
  readonly role: "user" | "assistant";
  /** The host's own timestamp for the message, in epoch milliseconds: the conversation order. */
  readonly at: number;
  readonly parts: readonly AgentStreamPart[];
  /** True while the message is still being produced. */
  readonly streaming: boolean;
  /**
   * The Dispatch message this user message delivered: set on the turn a person's direct message
   * from Dispatch became, so a viewer that also shows Dispatch's stored copy shows it once.
   */
  readonly dispatchMessageId?: string;
}

export interface AgentStreamToolResult {
  readonly toolCallId: string;
  readonly toolName: string;
  readonly at: number;
  readonly output: string;
  readonly isError: boolean;
}

/**
 * One frame. `seq` is the session's own monotonic counter: frames for one message arrive in any
 * order the host's concurrent handlers produce them, so a viewer applies a frame only when its
 * `seq` beats the one it already holds for that id.
 */
export type AgentStreamFrame =
  | {
      readonly v: typeof AGENT_STREAM_PROTOCOL;
      readonly kind: "message";
      readonly seq: number;
      readonly message: AgentStreamMessage;
    }
  | {
      readonly v: typeof AGENT_STREAM_PROTOCOL;
      readonly kind: "tool-result";
      readonly seq: number;
      readonly result: AgentStreamToolResult;
    };

/** What the relay asks a session for, and the ping that keeps it publishing. */
export interface AgentStreamControlMessage {
  readonly v: typeof AGENT_STREAM_PROTOCOL;
  readonly type: "replay" | "watch";
}

/**
 * Whether the session is answering the relay on its control subject, sent as the stream's
 * `responder` event whenever the answer changes and once at the start of every connection.
 *
 * Nobody answering is not an empty history: the session's plugin predates the live view, or the
 * session is not running. Only the session itself can tell the two apart, and it does so by
 * replying at all, which every release that can stream does. The relay retries the replay on
 * each watch tick while nobody answers, so a session restarted onto a newer plugin turns this
 * true and delivers its history without the viewer reloading.
 */
export interface AgentStreamResponder {
  readonly v: typeof AGENT_STREAM_PROTOCOL;
  readonly responding: boolean;
}

/** What a session answers a replay request with: everything it still holds, oldest first. */
export interface AgentStreamReplay {
  readonly v: typeof AGENT_STREAM_PROTOCOL;
  readonly session_id: string;
  readonly frames: readonly AgentStreamFrame[];
}

/** Cuts `text` to `limit` characters, marking it when it had to. */
export function capAgentStreamText(text: string, limit: number): string {
  if (text.length <= limit) return text;
  return text.slice(0, limit) + AGENT_STREAM_TRUNCATION_SUFFIX;
}
