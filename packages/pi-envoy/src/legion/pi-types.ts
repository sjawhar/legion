import type { EnvoySessionContext } from "../../extensions/envoy";

export interface SessionContext extends EnvoySessionContext {
  readonly taskDepth?: number;
  readonly sessionManager: {
    readonly getSessionId: () => string;
    readonly getSessionFile: () => string | undefined;
  };
}

export interface BeforeAgentStartEvent {
  readonly prompt: string;
}
export interface ToolCallEvent {
  readonly toolName: string;
  readonly toolCallId?: string;
  readonly input: Record<string, unknown>;
}

export interface ToolResultEvent {
  readonly toolName: string;
  readonly toolCallId: string;
  readonly input: Record<string, unknown>;
  readonly details: unknown;
  readonly isError: boolean;
}

export interface ToolCallEventResult {
  readonly block?: boolean;
  readonly reason?: string;
  readonly input?: Record<string, unknown>;
}

export interface CommandContext {
  readonly cwd: string;
  readonly sessionManager: { readonly getSessionId: () => string };
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
  readonly defaultInactive: boolean;
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
  readonly optional: () => unknown;
}

export interface ExtensionAgentsApi {
  readonly list: () => readonly { readonly id: string }[];
  readonly get: (agentId: string) => { readonly id: string } | undefined;
  readonly ensureLive: (
    agentId: string,
    options: { readonly parentSessionFile: string }
  ) => Promise<{ readonly id: string }>;
  readonly prompt: (agentId: string, content: string) => Promise<void>;
}

export interface PiApi {
  readonly zod: {
    readonly object: (shape: Readonly<Record<string, unknown>>) => unknown;
    readonly string: () => ZodProperty;
    readonly number: () => ZodProperty;
    readonly array: (item: unknown) => ZodProperty;
    readonly enum: (values: readonly string[]) => ZodProperty;
    readonly unknown: () => ZodProperty;
  };
  readonly agents?: ExtensionAgentsApi;
  readonly sendMessage: (message: { readonly type: string }) => void;
  readonly getActiveTools: () => readonly string[];
  readonly setActiveTools: (tools: string[]) => Promise<void>;
  readonly on: (
    event:
      | "session_start"
      | "before_agent_start"
      | "agent_end"
      | "tool_call"
      | "tool_result"
      | "session_shutdown",
    handler: (event: unknown, context: SessionContext) => Promise<unknown>
  ) => void;
  readonly registerTool: (tool: RegisteredTool) => void;
  readonly registerCommand: (
    name: string,
    command: {
      readonly description: string;
      readonly handler: (args: string, context: CommandContext) => Promise<void>;
    }
  ) => void;
}
