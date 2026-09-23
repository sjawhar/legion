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
