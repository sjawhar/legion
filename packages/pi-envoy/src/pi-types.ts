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
  readonly taskDepth?: number;
  readonly sessionManager: {
    readonly getSessionId: () => string;
    /**
     * Live display title. OMP assigns it after the first turn, so it is often
     * undefined at session_start.
     */
    readonly getSessionName?: () => string | undefined;
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
  };
  readonly setInterval: (callback: () => void, intervalMs: number) => void;
  readonly ui: {
    readonly notify: (message: string, level: "info" | "warning") => void;
  };
}

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

export interface AgentEndEvent {
  /** Set when OMP has already scheduled a continuation, so the turn is not settling. */
  readonly willContinue?: boolean;
}

export interface ToolCallEventResult {
  readonly block?: boolean;
  readonly reason?: string;
  readonly input?: Record<string, unknown>;
}

export interface ResourcesDiscoverResult {
  readonly skillPaths?: readonly string[];
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
    readonly result: undefined;
  };
  readonly agent_end: { readonly event: AgentEndEvent; readonly result: undefined };
  readonly tool_call: { readonly event: ToolCallEvent; readonly result: ToolCallEventResult };
  readonly tool_result: { readonly event: ToolResultEvent; readonly result: undefined };
}

export interface CommandContext {
  readonly cwd: string;
  readonly sessionManager: { readonly getSessionId: () => string };
  readonly ui: {
    readonly notify: (message: string, level: "info" | "warning") => void;
  };
}

export interface ToolResult {
  readonly content: readonly { readonly type: "text"; readonly text: string }[];
  readonly details: Readonly<Record<string, unknown>>;
  readonly isError?: boolean;
}

export interface RegisteredTool {
  readonly name: string;
  readonly label: string;
  readonly description: string;
  readonly defaultInactive?: boolean;
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
  readonly boolean?: () => ZodProperty;
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
 * MessageRenderer}. Envoy only ever sends string content (see
 * `PiApi["sendMessage"]` below), so this narrows the host's wider
 * `string | (TextContent | ImageContent)[]` union to what this package
 * actually produces.
 */
export interface RenderableMessage {
  readonly customType: string;
  readonly content: string;
}

export type MessageRenderer = (
  message: RenderableMessage,
  options: MessageRenderOptions,
  theme: MessageRendererTheme
) => Component | undefined;

export type { ExtensionAgentsApi };

export interface PiApi {
  readonly zod: PiZod;
  readonly agents?: ExtensionAgentsApi;
  readonly sendMessage: (
    message:
      | { readonly customType: string; readonly content: string; readonly display: boolean }
      | { readonly type: string },
    options?: { readonly deliverAs: "steer"; readonly triggerTurn: boolean }
  ) => void;
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
