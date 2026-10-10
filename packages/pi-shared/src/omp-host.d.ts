declare module "@oh-my-pi/pi-coding-agent" {
  export function copyToClipboard(text: string): Promise<void>;

  /** The host's process-wide roster of agent sessions (`packages/coding-agent/src/registry/agent-registry.ts`). */
  export type AgentKind = "main" | "sub" | "advisor";
  export interface AgentRef {
    readonly id: string;
    readonly kind: AgentKind;
    /** Null exactly when parked or aborted. */
    readonly session: { readonly sessionManager: { getSessionId(): string } } | null;
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
    readonly description: string;
    /** True when only the interactive terminal runs it, so `sendUserInput` answers `terminal-only`. */
    readonly terminalOnly: boolean;
  }
  export function listUserInputBuiltinCommands(): HostBuiltinCommand[];
}
