// Entry of the AskGate pre-send gate; the behaviour is described and implemented in
// askgate-core.ts. This file binds the Oh My Pi objects the core needs (the completion, the gate
// role's retry.fallbackChains chain, buildSessionContext for the primary's context, and the handler
// ceiling), so the core imports nothing from the host and runs under `bun test` with fakes.
//
// Every Oh My Pi module is loaded with a string-literal `import()` in `registerAskGate`, never a
// static import, as `ompCapabilityHost` loads them (packages/pi-legion/src/capability-report.ts):
// the host's loader resolves a bare `@oh-my-pi/*` specifier only when written as a literal, and a
// static import would fail `bun test` and the load of envoy.ts in any host without the module. The
// declarations are in packages/pi-shared/src/omp-host.d.ts.
//
// A transient provider failure gets one retry inside the gate's own deadline; a model that still
// fails hands the request to the next model of the chain, which the core walks. The chain comes
// from omp's own resolution (`resolveRetryFallbackChainKey` and `findRetryFallbackCandidates`,
// walked depth-first into each fallback's own chain as the eval `completion()` bridge walks it),
// keyed by the model role the gate used, because `ctx.models.resolve` returns the role's first
// model only.
import * as fs from "node:fs";
import * as os from "node:os";
import type { ApiKeyResolver, AssistantMessage, Model } from "@oh-my-pi/pi-ai";
import type { ModelLookupRegistry } from "@oh-my-pi/pi-coding-agent/config/model-resolver";
import type { RetryFallbackResolutionContext } from "@oh-my-pi/pi-coding-agent/session/retry-fallback-chains";
import type { SessionEntry } from "@oh-my-pi/pi-coding-agent/session/session-context";
import {
  type CompleteRequest,
  type Completion,
  createAskGate,
  GATE_EFFORT,
  type GateModel,
  type GateRole,
  type Message,
  type Pi,
  spawnDryRun,
  type Usage,
} from "./askgate-core";

/** What the entry hands the gate from its own extension: where the switch lives, and where notices go. */
export interface AskGateHost {
  /** The session's envoy.json, named in the notice for an OMP_ASKGATE the gate ignores. */
  readonly configFile: string;
  readonly logger: { warn(message: string): void };
}

// The core passes the host's objects through untyped: ctx is the ExtensionContext, whose
// modelRegistry and sessionManager this reads, and model is the full Model ctx.models.resolve
// handed back.
type Registry = ModelLookupRegistry & {
  resolver: (model: Model, sessionId: string) => ApiKeyResolver;
  find: (provider: string, id: string) => Model | undefined;
  hasProvider: (provider: string) => boolean;
};
type Ctx = {
  modelRegistry: Registry;
  sessionManager: { getEntries(): readonly SessionEntry[]; getLeafId(): string | null };
};

/** Appends to the dump, owner-only on every open (not only on creation), and never through a symlink. */
function appendDump(p: string, text: string): void {
  const fd = fs.openSync(
    p,
    fs.constants.O_WRONLY | fs.constants.O_APPEND | fs.constants.O_CREAT | fs.constants.O_NOFOLLOW,
    0o600
  );
  try {
    fs.fchmodSync(fd, 0o600);
    fs.writeSync(fd, text);
  } finally {
    fs.closeSync(fd);
  }
}

/**
 * Loads the Oh My Pi modules the gate needs, then binds it to `pi`. Rejects when a module does not
 * load; the caller reports that, since a gate that never binds lets every write out unreviewed.
 */
export async function registerAskGate(pi: Pi, host: AskGateHost): Promise<void> {
  // String-literal imports: the host's loader resolves only these (see the head of this file).
  const [
    { completeSimple, retryTransientCompletion },
    { buildSessionContext },
    { settings },
    { cfgDisabledProviders },
    { resolveModelOverride },
    { EXTENSION_HANDLER_TIMEOUT_MS },
    { cfgExtensionHandlersToolCallTimeoutMs },
    { findRetryFallbackCandidates, getRetryFallbackChains, resolveRetryFallbackChainKey },
    { cfgRetry },
  ] = await Promise.all([
    import("@oh-my-pi/pi-ai"),
    import("@oh-my-pi/pi-coding-agent/session/session-context"),
    import("@oh-my-pi/pi-coding-agent/config/settings"),
    import("@oh-my-pi/pi-coding-agent/config/model-settings"),
    import("@oh-my-pi/pi-coding-agent/config/model-resolver"),
    import("@oh-my-pi/pi-coding-agent/extensibility/extensions/runner"),
    import("@oh-my-pi/pi-coding-agent/extensibility/settings"),
    import("@oh-my-pi/pi-coding-agent/session/retry-fallback-chains"),
    import("@oh-my-pi/pi-coding-agent/session/settings"),
  ]);

  /** The models after `primary` in `role`'s chain, each followed by its own chain, deduplicated; none when model fallback is off. */
  const fallbackModels = (ctx: unknown, primary: GateModel, role: GateRole): GateModel[] => {
    const retry = cfgRetry.get(settings);
    if (!retry.enabled || !retry.modelFallback) return [];
    const registry = (ctx as Ctx).modelRegistry;
    const context: RetryFallbackResolutionContext = {
      chains: getRetryFallbackChains(settings),
      getModelRole: (name) => settings.getModelRole(name),
      modelLookup: registry,
    };
    const disabled = new Set(cfgDisabledProviders.get(settings));
    const seen = new Set([`${primary.provider}/${primary.id}`]);
    const expanded = new Set<string>();
    const out: Model[] = [];
    const walk = (selector: string, model: Model, roleHint: string | undefined) => {
      if (expanded.has(selector)) return;
      expanded.add(selector);
      const chainKey = resolveRetryFallbackChainKey(context, selector, model, roleHint);
      if (!chainKey) return;
      for (const entry of findRetryFallbackCandidates(context, chainKey, selector, model, {
        allowMissingPrimary: true,
      })) {
        const candidate = resolveModelOverride([entry.raw], registry, settings).model;
        if (
          !candidate ||
          disabled.has(candidate.provider) ||
          seen.has(`${candidate.provider}/${candidate.id}`)
        )
          continue;
        seen.add(`${candidate.provider}/${candidate.id}`);
        out.push(candidate);
        walk(entry.raw, candidate, undefined);
      }
    };
    walk(
      settings.getModelRole(role) ?? `${primary.provider}/${primary.id}`,
      primary as Model,
      role
    );
    return out;
  };

  const complete = async ({
    ctx,
    model,
    system,
    user,
    sessionId,
    signal,
  }: CompleteRequest): Promise<Completion> => {
    const full = model as Model;
    // Every attempt bills: a transient retry, a resampled thinking loop, the aborted last one. onAttempt sees each.
    const usage: Usage = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cost: 0 };
    const onAttempt = (attempt: AssistantMessage) => {
      usage.input += attempt.usage.input;
      usage.output += attempt.usage.output;
      usage.cacheRead += attempt.usage.cacheRead;
      usage.cacheWrite += attempt.usage.cacheWrite;
      usage.cost += attempt.usage.cost.total;
    };
    try {
      const message = await retryTransientCompletion(
        () =>
          completeSimple(
            full,
            {
              systemPrompt: [system],
              messages: [{ role: "user", content: user, timestamp: Date.now() }],
            },
            // Caching off: the provider's breakpoints cover the whole prompt, transcript included, so
            // every call would write it all at the one-hour rate (twice the input price here), and the
            // transcript, which slides on every call, is never read back.
            {
              apiKey: (ctx as Ctx).modelRegistry.resolver(full, sessionId),
              sessionId,
              reasoning: GATE_EFFORT,
              cacheRetention: "none",
              maxTokens: 1200,
              temperature: 0,
              signal,
              onAttempt,
            }
          ),
        { maxAttempts: 2, provider: full.provider, signal }
      );
      const text = message.content
        .filter((b): b is { type: "text"; text: string } => b.type === "text")
        .map((b) => b.text)
        .join("\n");
      return {
        text,
        error:
          message.stopReason === "error"
            ? (message.errorMessage ?? "error")
            : message.stopReason === "aborted"
              ? "aborted"
              : undefined,
        usage,
      };
    } catch (error) {
      // An abort during a retry's backoff, or a thrown provider error: what the attempts spent is still owed.
      return { text: "", error: error instanceof Error ? error.message : String(error), usage };
    }
  };

  createAskGate({
    env: process.env,
    home: os.homedir(),
    now: Date.now,
    logger: host.logger,
    configFile: host.configFile,
    // The dump holds the primary system prompt and transcript: owner-only, like the session files.
    appendFile: appendDump,
    complete,
    fallbackModels,
    // The runner's tool_call handler ceiling, normalised as the runner does (runner.ts normalizeHandlerTimeout).
    handlerCeilingMs: () => {
      const configured = cfgExtensionHandlersToolCallTimeoutMs.get(settings);
      return Number.isFinite(configured) && configured > 0
        ? configured
        : EXTENSION_HANDLER_TIMEOUT_MS;
    },
    dryRun: spawnDryRun,
    contextMessages: (ctx) => {
      const { sessionManager } = ctx as unknown as Ctx;
      return buildSessionContext([...sessionManager.getEntries()], sessionManager.getLeafId())
        .messages as unknown as readonly Message[];
    },
  })(pi);
}
