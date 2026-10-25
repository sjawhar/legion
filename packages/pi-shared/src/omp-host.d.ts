declare module "@oh-my-pi/pi-coding-agent" {
  export function copyToClipboard(text: string): Promise<void>;

  /** The host's process-wide roster of agent sessions (`packages/coding-agent/src/registry/agent-registry.ts`). */
  export type AgentKind = "main" | "sub" | "advisor";
  export interface AgentRef {
    readonly id: string;
    readonly kind: AgentKind;
    /** Null exactly when parked or aborted. These are `AgentSession` getters, absent on a build
     *  older than them. The pin's file commands and prompt templates both require descriptions. */
    readonly session: {
      readonly sessionManager: { getSessionId(): string };
      readonly slashCommands?: readonly { readonly name: string; readonly description: string }[];
      readonly promptTemplates?: readonly { readonly name: string; readonly description: string }[];
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
  export class Settings {
    /** The configured selector of a model role (`modelRoles.<role>`), or undefined when it is unset. */
    getModelRole(role: string): string | undefined;
  }
  export const settings: Settings;
}

declare module "@oh-my-pi/pi-coding-agent/extensibility/settings" {
  import type { Settings } from "@oh-my-pi/pi-coding-agent/config/settings";
  export const cfgDisabledExtensions: { get(settings: Settings): string[] };
  export const cfgSkills: { get(settings: Settings): Record<string, unknown> };
  /** `extensionHandlers.toolCallTimeoutMs`: the runner's ceiling for one `tool_call` handler. */
  export const cfgExtensionHandlersToolCallTimeoutMs: { get(settings: Settings): number };
}

/** The runner's default handler ceiling, used when `extensionHandlers.toolCallTimeoutMs` is unusable. */
declare module "@oh-my-pi/pi-coding-agent/extensibility/extensions/runner" {
  export const EXTENSION_HANDLER_TIMEOUT_MS: number;
}

declare module "@oh-my-pi/pi-coding-agent/config/model-settings" {
  import type { Settings } from "@oh-my-pi/pi-coding-agent/config/settings";
  export const cfgDisabledProviders: { get(settings: Settings): string[] };
}

declare module "@oh-my-pi/pi-coding-agent/config/model-resolver" {
  import type { Settings } from "@oh-my-pi/pi-coding-agent/config/settings";
  import type { Model } from "@oh-my-pi/pi-ai";
  export interface ModelLookupRegistry {
    getAvailable(): Model[];
  }
  /** The first model `modelPatterns` names, as a model role's value is resolved. */
  export function resolveModelOverride(
    modelPatterns: string[],
    modelRegistry: ModelLookupRegistry,
    settings?: Settings
  ): { readonly model?: Model };
}

declare module "@oh-my-pi/pi-coding-agent/session/settings" {
  import type { Settings } from "@oh-my-pi/pi-coding-agent/config/settings";
  /** The `retry.*` settings; only the two the fallback walk reads are declared. */
  export const cfgRetry: {
    get(settings: Settings): { readonly enabled: boolean; readonly modelFallback: boolean };
  };
}

/** The host's model fallback chains (`retry.fallbackChains`), resolved as a turn's recovery resolves them. */
declare module "@oh-my-pi/pi-coding-agent/session/retry-fallback-chains" {
  import type { Settings } from "@oh-my-pi/pi-coding-agent/config/settings";
  import type { Model } from "@oh-my-pi/pi-ai";
  export interface RetryFallbackModelLookup {
    find(provider: string, id: string): Model | undefined;
    hasProvider(provider: string): boolean;
  }
  export interface RetryFallbackResolutionContext {
    chains: Record<string, string[]>;
    getModelRole(role: string): string | undefined;
    modelLookup: RetryFallbackModelLookup;
  }
  export function getRetryFallbackChains(settings: Settings): Record<string, string[]>;
  /** The chain key (a model role or a model selector) whose chain follows `currentSelector`. */
  export function resolveRetryFallbackChainKey(
    context: RetryFallbackResolutionContext,
    currentSelector: string,
    currentModel?: Model | null,
    roleHint?: string
  ): string | undefined;
  export function findRetryFallbackCandidates(
    context: RetryFallbackResolutionContext,
    chainKey: string,
    currentSelector: string,
    currentModel?: Model | null,
    options?: { readonly allowMissingPrimary?: boolean; readonly wrapAround?: boolean }
  ): { readonly raw: string; readonly provider: string; readonly id: string }[];
}

declare module "@oh-my-pi/pi-coding-agent/session/session-context" {
  /** One entry of a session's tree; the context build reads it, so its shape is opaque here. */
  export interface SessionEntry {
    readonly id: string;
    readonly type: string;
  }
  /** The messages the session's model sees on the branch that ends at `leafId`. */
  export function buildSessionContext(
    entries: SessionEntry[],
    leafId?: string | null
  ): { readonly messages: readonly unknown[] };
}

/** The host's model client: one-shot completions, as an extension calls a model of its own. */
declare module "@oh-my-pi/pi-ai" {
  export type Api = string;
  export interface Model<TApi extends Api = Api> {
    readonly provider: string;
    readonly id: string;
    readonly api: TApi;
  }
  /** Resolves a request's API key when it is sent (`ModelRegistry.resolver`). */
  export type ApiKeyResolver = (context: never) => unknown;
  export interface AssistantMessage {
    readonly content: readonly { readonly type: string; readonly text?: string }[];
    readonly usage: {
      readonly input: number;
      readonly output: number;
      readonly cacheRead: number;
      readonly cacheWrite: number;
      readonly cost: { readonly total: number };
    };
    readonly stopReason: string;
    readonly errorMessage?: string;
  }
  export function completeSimple<TApi extends Api>(
    model: Model<TApi>,
    context: {
      readonly systemPrompt?: string[];
      readonly messages: { role: "user"; content: string; timestamp: number }[];
    },
    options?: {
      readonly apiKey?: string | ApiKeyResolver;
      readonly sessionId?: string;
      readonly reasoning?: string;
      readonly cacheRetention?: "none" | "short" | "long";
      readonly maxTokens?: number;
      readonly temperature?: number;
      readonly signal?: AbortSignal;
      /** Receives every completed result, including results retried by the thinking-loop guard. */
      readonly onAttempt?: (message: AssistantMessage) => void;
    }
  ): Promise<AssistantMessage>;
  /** Runs a one-shot completion again after a transient provider failure. */
  export function retryTransientCompletion(
    run: (attempt: number) => Promise<AssistantMessage>,
    options?: {
      readonly maxAttempts?: number;
      readonly provider?: string;
      readonly signal?: AbortSignal;
    }
  ): Promise<AssistantMessage>;
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

/** Our fork's typed-input module (can1357/oh-my-pi#14323); a host without it fails the import. */
declare module "@oh-my-pi/pi-coding-agent/extensibility/extensions/send-user-input-handler" {
  export interface UserInputBuiltinCommand {
    readonly name: string;
    /** The other names the host runs it by (`/models` for `/model`). */
    readonly aliases: readonly string[];
    readonly description: string;
    /** True when only the interactive terminal runs it, so `sendUserInput` answers `terminal-only`. */
    readonly terminalOnly: boolean;
  }
  export function listUserInputBuiltinCommands(): UserInputBuiltinCommand[];
}
