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

  /** The task agents the `task` tool resolves for `cwd` (`discoverAgents` in the agents module). */
  export function discoverAgents(
    cwd: string
  ): Promise<{ readonly agents: readonly { readonly name: string }[] }>;

  /** One web search through the configured provider (`runSearchQuery` in `web/search`). */
  export function runSearchQuery(
    args: { readonly query: string; readonly limit: number },
    options: { readonly sessionId?: string; readonly signal: AbortSignal }
  ): Promise<{
    readonly content?: readonly { readonly type: string; readonly text?: string }[];
    readonly details?: unknown;
  }>;

  /** The manager `discoverMCPServers` connects the configured servers through. */
  export interface MCPServerManager {
    waitForPendingConnections(): Promise<void>;
    getConnectionStatus(name: string): string;
    disconnectAll(): Promise<void>;
  }
  export function discoverMCPServers(
    cwd: string
  ): Promise<{ readonly manager: MCPServerManager; readonly errors: readonly unknown[] }>;

  /** Every skill loaded for `cwd` under the skills settings (`cfgSkills.get(settings)`). */
  export function loadSkills(options: {
    readonly cwd: string;
    readonly disabledExtensions: readonly string[];
    readonly [setting: string]: unknown;
  }): Promise<{ readonly skills: readonly { readonly filePath: string }[] }>;
}

declare module "@oh-my-pi/pi-coding-agent/config/settings" {
  /** The host's live settings object; the `cfg*` accessors read it. */
  export class Settings {}
  export const settings: Settings;
}

declare module "@oh-my-pi/pi-coding-agent/extensibility/settings" {
  import type { Settings } from "@oh-my-pi/pi-coding-agent/config/settings";
  export const cfgDisabledExtensions: { get(settings: Settings): string[] };
  export const cfgSkills: { get(settings: Settings): Record<string, unknown> };
}

declare module "@oh-my-pi/pi-coding-agent/extensibility/extensions/loader" {
  /** The module path of every extension loaded for `cwd`: the explicit paths, then the
   * discovered ones, less the disabled. */
  export function discoverExtensionPaths(
    explicit: readonly string[],
    cwd: string,
    disabled: readonly string[]
  ): Promise<string[]>;
}

declare module "@oh-my-pi/pi-coding-agent/mcp/config" {
  /** Every configured MCP server by name, and the file each configuration came from. */
  export function loadAllMCPConfigs(cwd: string): Promise<{
    readonly configs: Record<string, unknown>;
    readonly sources: Record<string, { readonly path: string }>;
  }>;
}

declare module "@oh-my-pi/pi-coding-agent/task/settings" {
  import type { Settings } from "@oh-my-pi/pi-coding-agent/config/settings";
  export const cfgTaskDisabledAgents: { get(settings: Settings): string[] };
}
