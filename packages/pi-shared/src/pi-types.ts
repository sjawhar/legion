import type { ExtensionAgentsApi } from "@oh-my-pi/pi-coding-agent";
import type { Component } from "@oh-my-pi/pi-tui";

/**
 * Oh My Pi host surface shared by both extension entries.
 *
 * This is the single declaration of the injected `pi` API and its contexts.
 * Both extensions/envoy.ts and extensions/legion.ts receive the same host
 * object; keep this contract complete instead of re-declaring partial copies
 * per extension — partial copies drift.
 */

export interface SessionContext {
  readonly cwd: string;
  /**
   * Whether the host gave this run a UI context. Measured on the pinned build: false for
   * `omp -p` and any other headless launch, true for a terminal and for an RPC host (every
   * Legion pane). A run with no UI is disposed when its one run ends, so nothing can act on a
   * message an extension sends at the stop. ACP supplies one too; there, a client that defers
   * agent-initiated turns has the host queue such a message as hidden next-turn context rather
   * than run a turn for it.
   */
  readonly hasUI: boolean;
  readonly taskDepth?: number;
  /** Which runner the event belongs to: the top-level session's is `main`, a `task` subagent's `sub`. */
  readonly agent?: { readonly kind: "main" | "sub" };
  readonly sessionManager: {
    readonly getSessionId: () => string;
    /**
     * Live display title: what `pi.setSessionName`, a rename, or OMP's title model last stored.
     * OMP titles a session (`auto`) from the first message typed at its terminal or given on its
     * command line; a Legion session sets its own at session_start
     * (`packages/pi-legion/src/session-title.ts`).
     */
    readonly getSessionName?: () => string | undefined;
    /**
     * The session header (`ReadonlySessionManager.getHeader`); `titleSource` says who set the
     * title: `auto` for OMP's title model, `user` for a rename or an extension's `setSessionName`.
     */
    readonly getHeader?: () => {
      readonly title?: string;
      readonly titleSource?: "auto" | "user";
    } | null;
    readonly getSessionFile: () => string | undefined;
    /**
     * Force the session's transcript onto disk even before it has an
     * assistant message. OMP allocates the session file path eagerly but
     * writes it lazily; a boot handshake that hands the path to the daemon
     * (for later resume/liveness probing) must call this first.
     */
    readonly ensureOnDisk: () => Promise<void>;
    /** Entries of the active branch; non-empty at session_start on resume. */
    readonly getBranch?: () => readonly unknown[];
    /**
     * Every entry the session holds, on every branch of its tree, in the order they were written
     * (the header excluded; `ReadonlySessionManager.getEntries`). An entry `pi.appendEntry` wrote
     * is in it at once, before the transcript reaches disk.
     */
    readonly getEntries: () => readonly unknown[];
  };
  readonly setInterval: (callback: () => void, intervalMs: number) => void;
  /** The host's managed one-shot timer: a throw or rejection is contained, cleared on shutdown. */
  readonly setTimeout: (callback: () => void | Promise<void>, delayMs: number) => void;
  /**
   * Oh My Pi 18.3 on: one side turn on the session's model over a snapshot of its conversation,
   * never added to the transcript. The host sends `promptText` as given. A context kept from
   * `session_start` may call it from later callbacks (upstream `docs/extensions.md`, "Ephemeral
   * side turns").
   */
  readonly runEphemeralTurn?: (options: {
    readonly promptText: string;
    readonly signal?: AbortSignal;
  }) => Promise<{ readonly replyText: string }>;
  readonly ui: {
    readonly notify: (message: string, level: "info" | "warning") => void;
  };
}

/** A side turn: one question to the session's model, answered without touching the transcript. */
export type SideTurn = (input: {
  readonly prompt: string;
  readonly signal?: AbortSignal;
}) => Promise<{ readonly replyText: string }>;

/**
 * Why OMP swapped the session under a running extension. `/new` and `/resume`
 * install a transcript that already matches its own id; `/fork` and `/handoff`
 * carry the current conversation into a freshly minted id.
 */
export type SessionSwitchReason = "new" | "resume" | "fork" | "handoff";

export interface SessionSwitchEvent {
  readonly reason: SessionSwitchReason;
}

export interface BeforeAgentStartEvent {
  readonly prompt: string;
}

export interface ExtensionMessage {
  readonly customType: string;
  readonly content: string;
  readonly display: boolean;
  readonly attribution?: "user" | "agent";
  /** Saved with the message and never sent to the model. */
  readonly details?: Readonly<Record<string, unknown>>;
}

export interface BeforeAgentStartResult {
  readonly message?: ExtensionMessage;
  readonly systemPrompt?: readonly string[];
}

export interface ToolCallEvent {
  readonly toolName: string;
  readonly toolCallId: string;
  readonly input: Record<string, unknown>;
}

export interface ToolResultEvent {
  readonly toolName: string;
  readonly toolCallId: string;
  readonly input: Record<string, unknown>;
  readonly details: unknown;
  readonly isError: boolean;
}

/** One of a run's messages, as far as an `agent_end` handler reads it. */
export interface AgentEndMessage {
  readonly role?: string;
  /** How an assistant reply ended: `stop` settled normally, `aborted` was an interrupt, `error` a
   * provider failure, `length` a truncation. */
  readonly stopReason?: string;
}

export interface AgentEndEvent {
  /** Set when OMP has already scheduled a continuation, so the turn is not settling. */
  readonly willContinue?: boolean;
  /** The run's messages; the last assistant entry says how the run ended. */
  readonly messages?: readonly AgentEndMessage[];
}

/** A message entering the session: a user prompt (the daemon's RPC `prompt` among them), a custom
 * message, an assistant reply, or a tool result. `message_update` carries the same shape for a
 * message still being produced — one per streamed delta, with the partial message — and
 * `message_end` for one that has settled. Measured on omp 18.3.2, the three are emitted
 * concurrently: a message's `message_end` can reach a handler before its `message_start`, so a
 * handler must not depend on their order. */
export interface MessageStartEvent {
  readonly message: unknown;
}

/** Fired when a top-level run — never a `task` subagent's — is about to settle at a final stop that
 * is not a tool call, after the host's own continuations (todo, plan, rewind) and never while an
 * async job's wake is pending. Only the members this package reads are typed. */
export interface SessionStopEvent {
  readonly last_assistant_message?: unknown;
  /** Aborted when the settle pass is (a user interrupt, a shutdown). */
  readonly signal: AbortSignal;
}

/** `continue` with `additionalContext` makes the host queue that text as one hidden message that
 * starts the next turn, instead of settling. */
export interface SessionStopEventResult {
  readonly continue: true;
  readonly additionalContext: string;
}

export interface ToolCallEventResult {
  readonly block?: boolean;
  readonly reason?: string;
  readonly input?: Record<string, unknown>;
}

export interface ResourcesDiscoverResult {
  readonly skillPaths?: readonly string[];
}

/** The messages one provider request is about to carry: a copy made for that request alone. */
export interface ContextEvent {
  readonly messages: readonly unknown[];
}

/** Replacement messages for that one request; Oh My Pi stores none of them in the session. */
export interface ContextEventResult {
  readonly messages?: readonly unknown[];
}

/** Text about to run as typed input: typed at the terminal, an RPC prompt, or `sendUserInput`. */
export interface InputEvent {
  readonly text: string;
  readonly images?: readonly ImageContent[];
  readonly source: "interactive" | "rpc" | "extension";
}

/** A handler may consume the text, or replace its text or images for every later step. */
export interface InputEventResult {
  readonly handled?: boolean;
  readonly text?: string;
  readonly images?: readonly ImageContent[];
}

/**
 * Payload and result type of every host event these extensions subscribe to.
 * OMP declares `on` as one overload per event name; mirroring that here keeps a
 * handler from reading a field its event does not carry, or from returning a
 * result shape the host discards. Payloads no handler reads stay `unknown`.
 */
export interface PiEventContract {
  readonly resources_discover: {
    readonly event: unknown;
    readonly result: ResourcesDiscoverResult;
  };
  readonly session_start: { readonly event: unknown; readonly result: undefined };
  readonly session_switch: { readonly event: SessionSwitchEvent; readonly result: undefined };
  readonly session_branch: { readonly event: unknown; readonly result: undefined };
  readonly session_tree: { readonly event: unknown; readonly result: undefined };
  readonly session_shutdown: { readonly event: unknown; readonly result: undefined };
  readonly before_agent_start: {
    readonly event: BeforeAgentStartEvent;
    readonly result: BeforeAgentStartResult;
  };
  /** Every run the host starts, the user's prompt or not; it carries nothing an extension reads. */
  readonly agent_start: { readonly event: unknown; readonly result: undefined };
  readonly agent_end: { readonly event: AgentEndEvent; readonly result: undefined };
  readonly message_start: { readonly event: MessageStartEvent; readonly result: undefined };
  readonly message_update: { readonly event: MessageStartEvent; readonly result: undefined };
  readonly message_end: { readonly event: MessageStartEvent; readonly result: undefined };
  /** Every provider request the session's agent loop sends, each turn and each tool round. */
  readonly context: { readonly event: ContextEvent; readonly result: ContextEventResult };
  /** Every input before it runs: the host's typed-input ingress, `sendUserInput` included. */
  readonly input: { readonly event: InputEvent; readonly result: InputEventResult };
  readonly session_stop: {
    readonly event: SessionStopEvent;
    readonly result: SessionStopEventResult;
  };
  readonly tool_call: { readonly event: ToolCallEvent; readonly result: ToolCallEventResult };
  readonly tool_result: { readonly event: ToolResultEvent; readonly result: undefined };
}

/** The context OMP hands a registered slash-command handler. Its `sessionManager` is the same
 * live object a `SessionContext` carries; only the members this package reads are typed. */
export interface CommandContext {
  readonly cwd: string;
  readonly sessionManager: Pick<
    SessionContext["sessionManager"],
    "getSessionId" | "getSessionFile" | "ensureOnDisk"
  >;
  readonly ui: {
    readonly notify: (message: string, level: "info" | "warning") => void;
  };
}

/** The host's text block (`TextContent` in `@oh-my-pi/pi-ai`), as far as this package writes it. */
export interface TextContent {
  readonly type: "text";
  readonly text: string;
}

/** The host's image block (`ImageContent` in `@oh-my-pi/pi-ai`): base64 bytes and their type. */
export interface ImageContent {
  readonly type: "image";
  readonly data: string;
  readonly mimeType: string;
}

/** What a tool result, a custom message and a user prompt may carry (`AgentToolResult.content`,
 *  `CustomMessageContent`, `sendUserMessage`'s array form). */
export type ContentBlock = TextContent | ImageContent;

export interface ToolResult {
  readonly content: readonly ContentBlock[];
  readonly details: Readonly<Record<string, unknown>>;
  readonly isError?: boolean;
}

export interface RegisteredTool {
  readonly name: string;
  readonly label: string;
  readonly description: string;
  readonly defaultInactive?: boolean;
  /**
   * Hand the raw arguments to `execute` when the host's own schema validation fails, so
   * the tool refuses a bad call once with every problem in its own words.
   */
  readonly lenientArgValidation?: boolean;
  readonly parameters: unknown;
  readonly execute: (
    id: string,
    parameters: Record<string, unknown>,
    signal: AbortSignal | undefined,
    onUpdate: unknown,
    context: SessionContext
  ) => Promise<ToolResult>;
}

export interface ZodProperty {
  readonly optional: () => ZodProperty;
  readonly nullable: () => ZodProperty;
  readonly describe: (description: string) => ZodProperty;
  readonly min?: (value: number) => ZodProperty;
  readonly max?: (value: number) => ZodProperty;
}

export interface ZodNumberProperty extends ZodProperty {
  readonly int: () => ZodProperty;
}

export interface PiZod {
  readonly object: (shape: Readonly<Record<string, unknown>>) => unknown;
  readonly string: () => ZodProperty;
  readonly number: () => ZodNumberProperty;
  readonly boolean: () => ZodProperty;
  readonly array: (item: unknown) => ZodProperty;
  readonly enum: (values: readonly string[]) => ZodProperty;
  readonly unknown: () => ZodProperty;
}

export interface MessageRenderOptions {
  readonly expanded: boolean;
}

/**
 * The slice of the host `Theme` a message renderer needs to paint a bordered
 * card: color/style helpers for exactly the tokens this package renders
 * with, and the rounded box-drawing glyphs.
 */
export interface MessageRendererTheme {
  readonly fg: (
    color: "borderMuted" | "customMessageLabel" | "customMessageText",
    text: string
  ) => string;
  readonly bold: (text: string) => string;
  readonly boxRound: {
    readonly topLeft: string;
    readonly topRight: string;
    readonly bottomLeft: string;
    readonly bottomRight: string;
    readonly horizontal: string;
    readonly vertical: string;
  };
}

/**
 * Extension-injected message entry handed to a registered {@link
 * MessageRenderer}: the host's `string | (TextContent | ImageContent)[]`.
 */
export interface RenderableMessage {
  readonly customType: string;
  readonly content: string | readonly ContentBlock[];
}

export type MessageRenderer = (
  message: RenderableMessage,
  options: MessageRenderOptions,
  theme: MessageRendererTheme
) => Component | undefined;

export type { ExtensionAgentsApi };

/**
 * What `sendUserInput` did with its text: `prompt` and `skill` submitted a message carrying the
 * caller's tag; `command` ran something locally (`output` is what a built-in printed, and
 * `agentInvoked` says it started a turn without a message, as `/retry` does); `terminal-only` is a
 * built-in only the interactive terminal runs (`/new`, `/resume`), and nothing ran; `unavailable`
 * is a host mode that does not wire the method, or a guest in a shared session, and nothing ran.
 */
export interface UserInputResult {
  readonly handled: "prompt" | "command" | "skill" | "terminal-only" | "unavailable";
  readonly output?: string;
  readonly agentInvoked?: boolean;
}

export interface HostSlashCommand {
  readonly name: string;
  readonly description?: string;
  readonly source: "extension" | "prompt" | "skill";
}

export interface PiApi {
  readonly zod: PiZod;
  readonly agents?: ExtensionAgentsApi;
  readonly sendMessage: (
    message:
      | {
          readonly customType: string;
          readonly content: string | readonly ContentBlock[];
          readonly display: boolean;
          readonly details?: Readonly<Record<string, unknown>>;
        }
      | { readonly type: string },
    options?: { readonly deliverAs: "steer" | "aside"; readonly triggerTurn: boolean }
  ) => void;
  /**
   * Sends a user prompt, exactly as Enter at the terminal does: idle, it starts a turn; streaming,
   * it steers. `deliverAs: "aside"` lands it at the next step without interrupting the running
   * tool batch. The host queues the send, so the prompt's `message_start` comes after this returns.
   */
  readonly sendUserMessage: (
    content: string | readonly ContentBlock[],
    options?: { readonly deliverAs: "aside" }
  ) => void;
  /**
   * Runs text as if typed at the session's own terminal: a built-in its host can run headless, an
   * extension or custom command, a file command or prompt template, and `/skill:<name>`; any other
   * text goes to the model as a prompt. `tag` is recorded on the message the input submits and
   * seen again on its `message_start`. Our Oh My Pi fork carries it (can1357/oh-my-pi#14323); a host
   * without it has no such member. On an idle session it resolves only once the run it started
   * ends, so a caller that must keep taking messages never awaits it in line.
   */
  readonly sendUserInput?: (
    text: string,
    options?: { readonly deliverAs?: "aside"; readonly tag?: string }
  ) => Promise<UserInputResult>;
  /** The session's extension, custom and skill slash commands (`skill:<name>`), never its
   *  built-ins. */
  readonly getCommands: () => readonly HostSlashCommand[];
  /** Persist extension state in the session transcript; never sent to the model. */
  readonly appendEntry: <T = unknown>(customType: string, data?: T) => void;
  /**
   * Sets the session's display title and persists it in the transcript, so a `--resume` keeps it.
   * The host stores it with `titleSource: "user"`, which OMP's own title model never overwrites.
   */
  readonly setSessionName: (name: string) => Promise<void>;
  readonly getActiveTools: () => readonly string[];
  readonly setActiveTools: (tools: string[]) => Promise<void>;
  readonly on: <Event extends keyof PiEventContract>(
    event: Event,
    handler: (
      event: PiEventContract[Event]["event"],
      context: SessionContext
    ) => Promise<PiEventContract[Event]["result"] | undefined> | Promise<void>
  ) => void;
  readonly registerTool: (tool: RegisteredTool) => void;
  readonly registerCommand: (
    name: string,
    command: {
      readonly description: string;
      readonly handler: (args: string, context: CommandContext) => Promise<void>;
    }
  ) => void;
  readonly registerMessageRenderer: (customType: string, renderer: MessageRenderer) => void;
}
