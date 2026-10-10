declare module "@oh-my-pi/pi-coding-agent" {
  export function copyToClipboard(text: string): Promise<void>;

  /** The host's process-wide roster of agent sessions (`packages/coding-agent/src/registry/agent-registry.ts`). */
  export type AgentKind = "main" | "sub" | "advisor";
  /** A file command (`slashCommands`) or prompt template (`promptTemplates`) the live session
   *  expands in `prompt()`; the host's own entries carry more fields than these. */
  export interface SessionPromptCommand {
    readonly name: string;
    readonly description?: string;
  }
  export interface AgentRef {
    readonly id: string;
    readonly kind: AgentKind;
    /** Null exactly when parked or aborted. The two lists are `AgentSession`'s getters
     *  (`session/agent-session.ts`), absent on a build older than them. */
    readonly session: {
      readonly sessionManager: { getSessionId(): string };
      readonly slashCommands?: readonly SessionPromptCommand[];
      readonly promptTemplates?: readonly SessionPromptCommand[];
    } | null;
    readonly sessionFile: string | null;
  }
  export class AgentRegistry {
    static global(): AgentRegistry;
    list(): AgentRef[];
  }

  export interface ExtensionAgent {
    readonly id: string;
  }

  export interface ExtensionAgentsApi {
    list(): readonly ExtensionAgent[];
    get(agentId: string): ExtensionAgent | undefined;
    ensureLive(
      agentId: string,
      options: { readonly parentSessionFile: string }
    ): Promise<ExtensionAgent>;
    prompt(agentId: string, content: string): Promise<void>;
  }
}

/** Our fork's typed-input module (can1357/oh-my-pi#14323); a host without it fails the import. */
declare module "@oh-my-pi/pi-coding-agent/extensibility/extensions/send-user-input-handler" {
  export interface HostBuiltinCommand {
    readonly name: string;
    /** The other names the host runs it by (`/models` for `/model`). */
    readonly aliases: readonly string[];
    readonly description: string;
    /** True when only the interactive terminal runs it, so `sendUserInput` answers `terminal-only`. */
    readonly terminalOnly: boolean;
  }
  export function listUserInputBuiltinCommands(): HostBuiltinCommand[];
}
