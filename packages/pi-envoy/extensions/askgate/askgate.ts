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
import { selfOwner, setRecordMode } from "@legion/envoy-client/askgate-marker";
import {
  sessionDirectory,
  sessionStateRoot,
} from "@legion/envoy-client/dispatch-session-directory";
import type { SessionConfigFile } from "@legion/envoy-client/session-settings";
import type { PiApi, PiEventContract } from "@legion/pi-shared/pi-types";
import type { ApiKeyResolver, AssistantMessage, Model } from "@oh-my-pi/pi-ai";
import type { ModelLookupRegistry } from "@oh-my-pi/pi-coding-agent/config/model-resolver";
import type { RetryFallbackResolutionContext } from "@oh-my-pi/pi-coding-agent/session/retry-fallback-chains";
import type { SessionEntry } from "@oh-my-pi/pi-coding-agent/session/session-context";
import { sessionDispatchClient } from "../../src/dispatch-session";
import {
  type CompleteRequest,
  type Completion,
  createAskGate,
  GATE_EFFORT,
  type GateCtx,
  type GateModel,
  type GateRole,
  loadFailureGate,
  type Message,
  type Pi,
  spawnDryRun,
  type ToolCallEvent,
  type ToolCallResult,
} from "./askgate-core";
import {
  createDecisionRecorder,
  type DecisionRecorder,
  type RecordRequest,
} from "./decision-record";
import type { Usage } from "./rules-check";

/** What the entry hands the gate from its own extension: where the switch lives, and where notices go. */
export interface AskGateHost {
  /**
   * The session's envoy.json, found and read once by the extension that loads the gate: its path is
   * named in the notice for an OMP_ASKGATE the gate ignores, and its Dispatch section is what the
   * gate's own reads of a document and its decision records go through.
   */
  readonly configFile: Extract<SessionConfigFile, { error: null }>;
  /** Where the gate's bind-time warning goes: the host shows it again at each session start. */
  readonly logger: { warn(message: string): void };
  /** The host's plain log, for a warning one session's gated calls raise while it runs. */
  readonly runtimeLogger: { warn(message: string): void };
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

type ToolCallHandler = (event: ToolCallEvent, ctx: GateCtx) => Promise<ToolCallResult>;

/**
 * What the entry gets back from `registerAskGate`: the load, and the recorder for a write the
 * gate never saw, which the `dispatch` CLI logged in the session's ledger.
 */
export interface AskGateRegistration {
  /** Settles with the gate's load and rejects with its error, for the caller's notice. */
  readonly loaded: Promise<void>;
  /**
   * Publishes one Dispatch decision record for a write that reached the CLI outside the form the
   * gate reads (`decision-record.ts`). The write runs until Dispatch has answered it or the
   * session's shutdown cuts it, and the caller awaits it outside its own handler. Answers the
   * text for the `recordError` of the `advisor-gate-record` follow-up the caller then appends
   * when the write failed or was given up, and undefined when it recorded or when the entry is
   * nothing to record.
   */
  readonly recordUnmarked: (request: RecordRequest) => Promise<string | undefined>;
}

/**
 * Registers AskGate's one `tool_call` handler on `pi` now, synchronously, so no call can pass
 * between the load of Oh My Pi's modules and the gate's binding, and starts that load. Each call
 * waits for the load. Loaded, the handler runs the core's own; not loaded, it refuses each write
 * the gate would review (`loadFailureGate`), since only `askgate.enabled: false` turns AskGate off.
 */
export function registerAskGate(pi: PiApi, host: AskGateHost): AskGateRegistration {
  // One recorder per process for the core's records and the unmarked writes' alike: each session
  // has one decision document, and a second recorder would upload it again under its name, which
  // replaces it. The unmarked writes' record needs none of Oh My Pi's modules, so it works whether
  // or not the load below settles.
  const record = createDecisionRecorder({
    env: process.env,
    now: Date.now,
    dispatchClient: (directory, signal) =>
      sessionDispatchClient(host.configFile, directory, signal),
  });
  const loading = loadAskGate(pi, host, record);
  // A rejection is handled here and by the caller's notice; never an unhandled one.
  const gate: Promise<ToolCallHandler> = loading.then(
    (handler) => handler,
    (error: unknown) => {
      const refuse = loadFailureGate(
        error,
        { env: process.env, configFile: host.configFile.path },
        pi
      );
      return async (event, ctx) => refuse(event, ctx);
    }
  );
  // The host's context carries every member GateCtx reads; PiApi types only those the Envoy entry
  // reads, so the handler takes it as the core's type.
  pi.on("tool_call", async (event, ctx) => (await gate)(event, ctx as unknown as GateCtx));
  // The one owner of the records' shutdown, registered here at bind time and never anywhere else:
  // the recorder holds the cut and its delay (`shutdown`), so the records of a gate that never
  // loaded — the unmarked writes', which need none of Oh My Pi's modules — are given the same
  // window as the core's own, and the core only waits on what is still pending.
  pi.on("session_shutdown", async () => {
    await record.shutdown();
  });
  return {
    loaded: loading.then(() => undefined),
    recordUnmarked: (request) => record.record(request),
  };
}

/**
 * Loads the Oh My Pi modules the gate needs, then binds the core to `pi` with its `tool_call`
 * handler kept back, which it returns: every other event, and `appendEntry`, reach the real `pi`.
 * Rejects when a module does not load or the core refuses to bind.
 */
async function loadAskGate(
  pi: PiApi,
  host: AskGateHost,
  record: DecisionRecorder
): Promise<ToolCallHandler> {
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
              // Adaptive thinking spends this budget too: a revise that rewrote a 1,450-character
              // question used 1,106 output tokens, thinking included. An answer cut off holds no
              // verdict and the write posts unreviewed, so this leaves room for a full
              // 2,000-character rewrite (ASK_QUESTION_MAX).
              maxTokens: 4096,
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

  // The core binds to a `pi` that keeps its tool_call handler, which the one registered above
  // runs, and passes every other event, and appendEntry, to the real `pi`. The core names its
  // events as strings, each one in PiEventContract, and types its handlers' events itself.
  let handler: ToolCallHandler | undefined;
  const wrapped: Pi = {
    on: (event, listener) => {
      if (event === "tool_call") handler = listener as unknown as ToolCallHandler;
      else pi.on(event as keyof PiEventContract, listener as unknown as () => Promise<void>);
    },
    appendEntry: (customType, data) => pi.appendEntry(customType, data),
  };
  // This process as the owner of each session's record (`selfOwner`): its marker store, and the
  // owner file setRecordMode rewrites with the current session's mode.
  const self = selfOwner(sessionStateRoot(process.env));
  createAskGate({
    env: process.env,
    home: os.homedir(),
    now: Date.now,
    logger: host.logger,
    runtimeLogger: host.runtimeLogger,
    configFile: host.configFile.path,
    dispatchClient: (directory, signal) =>
      sessionDispatchClient(host.configFile, directory, signal),
    recordDecision: record,
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
    // The store envoy.ts names in the session's record: the CLI state root's, for this process.
    markerStore: self.store,
    // Records the check mode into the record of the session this process is bound to now, which
    // envoy.ts named in DISPATCH_SESSION_ID. Keyed to the session, so a switch never changes
    // another session's mode; a process with no bound session (none to check) records nothing.
    recordCheckMode: (mode) => {
      const id = process.env.DISPATCH_SESSION_ID;
      if (id === undefined || id === "") return;
      setRecordMode(sessionDirectory(process.env, id), self, mode);
    },
    contextMessages: (ctx) => {
      const { sessionManager } = ctx as unknown as Ctx;
      return buildSessionContext([...sessionManager.getEntries()], sessionManager.getLeafId())
        .messages as unknown as readonly Message[];
    },
  })(wrapped);
  if (handler === undefined) throw new Error("AskGate's core registered no tool_call handler");
  return handler;
}
