// The stand-in Oh My Pi host `extensions/envoy.ts` is driven against off a real session: the
// surface the extension sees (`TestPi`) and a recorder of everything it registers, sends and
// appends. Two callers share it, which is why it is a module of its own: the package's own suite
// (`extensions/envoy.test.ts`, through its `createPi`, which adds the clipboard the suite mocks)
// and `scripts/smoke-role-takeover.sh`'s client process
// (`scripts/role-takeover/omp-client.ts`), one client instance per OS process. A host method the
// extension starts to use is added here once, so the smoke cannot drift behind the suite.
import type { MessageRenderer, PiApi } from "@legion/pi-shared/pi-types";
import { z } from "zod";

export type ToolResult = {
  readonly content: readonly { readonly type: "text"; readonly text: string }[];
  readonly details: Readonly<Record<string, unknown>>;
  readonly isError?: boolean;
};

export type RegisteredTool = {
  readonly name: string;
  readonly description: string;
  readonly parameters: unknown;
  readonly execute: (
    id: string,
    params: Record<string, unknown>,
    signal?: AbortSignal,
    onUpdate?: unknown,
    context?: SessionContext
  ) => Promise<ToolResult>;
};

export type RegisteredCommand = {
  readonly name: string;
  readonly description: string | undefined;
  readonly handler: (args: string, context: CommandContext) => Promise<void>;
};

export type SessionContext = {
  readonly cwd: string;
  /** Oh My Pi's agent identity: the top-level session's runner is `main`, a `task` subagent's `sub`. */
  readonly agent?: { readonly kind: "main" | "sub" };
  readonly sessionManager: {
    readonly getSessionId: () => string;
    readonly getSessionName?: () => string | undefined;
    readonly getBranch?: () => readonly unknown[];
    readonly getEntries: () => readonly unknown[];
    readonly getSessionFile: () => string | undefined;
    readonly ensureOnDisk: () => Promise<void>;
  };
  readonly setInterval: (callback: () => void, intervalMs: number) => void;
  /** The host's `ctx.isIdle()`: false while a turn runs. */
  readonly isIdle?: () => boolean;
  readonly ui: {
    readonly notify: (message: string, level: "info" | "warning") => void;
    readonly onTerminalInput: (handler: (input: unknown) => void) => () => void;
    readonly getEditorText: () => string;
  };
};

export type CommandContext = {
  readonly ui: {
    readonly notify: (message: string, level: "info" | "warning" | "error") => void;
  };
};

export type TestPi = {
  readonly zod: typeof z;
  readonly registerTool: (tool: RegisteredTool) => void;
  readonly registerCommand: (name: string, command: Omit<RegisteredCommand, "name">) => void;
  readonly registerMessageRenderer: PiApi["registerMessageRenderer"];
  readonly on: (
    event:
      | "resources_discover"
      | "session_start"
      | "session_switch"
      | "session_branch"
      | "session_tree"
      | "session_shutdown"
      | "before_agent_start"
      | "agent_end"
      | "message_start"
      | "session_stop"
      | "input"
      | "tool_call"
      | "tool_result",
    handler: (event: unknown, context: SessionContext) => Promise<unknown>
  ) => void;
  readonly sendMessage: (
    message: { readonly content: string; readonly customType?: string; readonly display?: boolean },
    options: unknown
  ) => void;
  readonly sendUserMessage: (content: string, options?: unknown) => void;
  readonly appendEntry: PiApi["appendEntry"];
};

/** A message the extension sent through `pi.sendMessage`, as the host records it. */
export type Delivery = {
  readonly content: string;
  readonly customType?: string;
  readonly details?: unknown;
  readonly display?: boolean;
  readonly options: unknown;
};

/** An event handler the extension registered through `pi.on`. */
export type EventHandler = (event: unknown, context: SessionContext) => Promise<unknown>;

/** A custom entry the extension persisted, in the shape `getBranch()` and `getEntries()` return. */
export type BranchEntry = {
  readonly type: "custom";
  readonly customType: string;
  readonly data: unknown;
};

/**
 * One stand-in host: `pi`, to hand the extension, and the arrays and maps it records into. The
 * caller reads `tools` and `handlers` to drive the extension, and `entries` is also the branch a
 * session manager hands back (`omp-client.ts`'s does).
 *
 * `zod` replaces the `z` the extension validates with, for the one case that injects another.
 */
export function createStandInHost(options: { readonly zod?: typeof z } = {}) {
  const commands: RegisteredCommand[] = [];
  const tools: RegisteredTool[] = [];
  const handlers = new Map<string, EventHandler>();
  const renderers = new Map<string, MessageRenderer>();
  const messages: string[] = [];
  const deliveries: Delivery[] = [];
  // User turns the extension started, in the order it sent them.
  const userMessages: { readonly content: string; readonly options: unknown }[] = [];
  const entries: BranchEntry[] = [];
  const pi: TestPi = {
    zod: options.zod ?? z,
    registerTool: (tool) => tools.push(tool),
    registerCommand: (name, command) => commands.push({ name, ...command }),
    registerMessageRenderer: (customType, renderer) => renderers.set(customType, renderer),
    on: (event, handler) => handlers.set(event, handler),
    sendMessage: (message, options) => {
      messages.push(message.content);
      deliveries.push({
        content: message.content,
        customType: message.customType,
        details: "details" in message ? message.details : undefined,
        display: message.display,
        options,
      });
    },
    sendUserMessage: (content, options) => {
      userMessages.push({ content, options });
    },
    appendEntry: (customType, data) => {
      entries.push({ type: "custom", customType, data });
    },
  };
  return {
    commands,
    deliveries,
    entries,
    handlers,
    messages,
    pi,
    renderers,
    tools,
    userMessages,
  };
}
