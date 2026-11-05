import { messageFor } from "@legion/envoy-client/errors";
import type { Model } from "@oh-my-pi/pi-ai";
import type {
  RetryFallbackModelLookup,
  RetryFallbackResolutionContext,
} from "@oh-my-pi/pi-coding-agent/session/retry-fallback-chains";
import type { HostModel } from "./pi-types";

/**
 * Which model is below this one in the deployment's own `retry.fallbackChains` (INFRA-956 R1), so
 * a worker whose turn a content filter refused steps down the same chain the host's own recovery
 * would have stepped down.
 *
 * Every Oh My Pi module is loaded with a string-literal `import()` inside the call, never a static
 * import, as `ompCapabilityHost` loads them (`packages/pi-legion/src/capability-report.ts`) and as
 * the AskGate entry loads its own (`packages/pi-envoy/extensions/askgate/askgate.ts:170-190`): the
 * host's loader resolves a bare `@oh-my-pi/*` specifier only when it is written as a literal, and
 * a static import would load Oh My Pi under `bun test`. The declarations are in `src/omp-host.d.ts`.
 *
 * It never throws out of a host event handler: a module that does not load, and a chain that does
 * not read, are one warning and no model.
 */

/** The log a failed load or read writes its `warn` to (`logger` from `@oh-my-pi/pi-utils` in
 * production). */
export interface FallbackChainLog {
  readonly warn: (message: string, context?: Record<string, unknown>) => void;
}

/** The slice of the host's session context the chain walk reads. */
export interface FallbackChainContext {
  /** The host's `ctx.modelRegistry`, which the walk hands the chain resolution as its
   * `modelLookup`, exactly as the AskGate entry hands it the same object
   * (`packages/pi-envoy/extensions/askgate/askgate.ts:196-200`). */
  readonly modelRegistry: RetryFallbackModelLookup;
  /** The host's `ctx.models.resolve`: what one chain entry's selector resolves to for this
   * session, or undefined when this session can use no such model. */
  readonly models: { readonly resolve: (spec: string) => HostModel | undefined };
  readonly log: FallbackChainLog;
}

/** The `provider/id` selector of a model, which is how a chain entry names one and how the
 * cooldown keys a model's own doubling level (`./fallback-cooldown`). */
export function modelSelector(model: HostModel): string {
  return `${model.provider}/${model.id}`;
}

/**
 * The model below `model` in the deployment's chain, or undefined when there is none: when the
 * deployment turned `retry.enabled` or `retry.modelFallback` off, when no chain key covers this
 * model, when its chain names nothing after it, or when the next entry resolves to no model this
 * session can use. `roleHint` is the model role the session runs under, which the host's own
 * resolution prefers over the selector when it keys the chain.
 */
export async function nextFallbackModel(
  ctx: FallbackChainContext,
  model: HostModel,
  roleHint?: string
): Promise<HostModel | undefined> {
  try {
    // String-literal imports: the host's loader resolves only these (see the head of this file).
    const [
      { findRetryFallbackCandidates, getRetryFallbackChains, resolveRetryFallbackChainKey },
      { settings },
      { cfgRetry },
    ] = await Promise.all([
      import("@oh-my-pi/pi-coding-agent/session/retry-fallback-chains"),
      import("@oh-my-pi/pi-coding-agent/config/settings"),
      import("@oh-my-pi/pi-coding-agent/session/settings"),
    ]);
    const retry = cfgRetry.get(settings);
    if (!retry.enabled || !retry.modelFallback) return undefined;
    const context: RetryFallbackResolutionContext = {
      chains: getRetryFallbackChains(settings),
      getModelRole: (name) => settings.getModelRole(name),
      modelLookup: ctx.modelRegistry,
    };
    const selector = modelSelector(model);
    // The chain matches the session's model by identity as well as by selector, so the resolution
    // is given both, as the host's own recovery gives them.
    const current = model as Model;
    const chainKey = resolveRetryFallbackChainKey(context, selector, current, roleHint);
    if (chainKey === undefined) return undefined;
    const next = findRetryFallbackCandidates(context, chainKey, selector, current)[0];
    return next === undefined ? undefined : ctx.models.resolve(next.raw);
  } catch (error) {
    ctx.log.warn("legion: the model fallback chain did not resolve; staying on this model", {
      model: modelSelector(model),
      error: messageFor(error),
    });
    return undefined;
  }
}
