import { messageFor } from "@legion/envoy-client/errors";
import type { FallbackChainContext, FallbackChainLog } from "@legion/pi-shared/fallback-chain";
import {
  type FallbackCooldownInput,
  type FallbackCooldownLimits,
  type FallbackCooldownState,
  restoreDue,
  stepFallbackCooldown,
} from "@legion/pi-shared/fallback-cooldown";
import type {
  AgentEndEvent,
  AgentEndMessage,
  HostModel,
  ModelChangeEntry,
  RetryFallbackAppliedEvent,
  SessionContext,
} from "@legion/pi-shared/pi-types";
import { classifyProviderError, classifyProviderTurn } from "@legion/pi-shared/provider-refusal";

/**
 * The session's model across a provider's refusal (INFRA-956 R1): a content filter that refuses a
 * Legion role's turn sends the next request to the next model of Oh My Pi's fallback chain, in
 * the same session, and the session comes back to its own model after a cooldown that grows each
 * time that model is refused again.
 *
 * Every collaborator is a field of `ModelFallbackDeps`, the shape
 * `packages/pi-shared/src/phase-reminder.ts` has: this module imports nothing of Oh My Pi, so a
 * unit test drives it with no host, and `packages/pi-legion/extensions/legion.ts` is the only
 * file that knows what `pi` is. The waits are decided by the cooldown machine
 * (`@legion/pi-shared/fallback-cooldown`); this runner owns the host's events and the switch.
 *
 * Nothing here throws out of a host event handler: every failure is one `log.warn` and a return.
 */

/** The slice of the host's session context this runner reads; a `SessionContext`
 * (`@legion/pi-shared/pi-types`) is one, and a unit test builds one by hand. */
export interface ModelFallbackContext {
  readonly sessionManager: { readonly getBranch?: () => readonly unknown[] };
  readonly models?: SessionContext["models"];
  readonly modelRegistry?: SessionContext["modelRegistry"];
}

export interface ModelFallbackDeps {
  /** `pi.setModel`: switches the session's model as `/model` does, and answers false when no
   * credential of this session serves it. */
  readonly setModel: (model: HostModel) => Promise<boolean>;
  /** Persists the state where a relaunch of this worker reads it again (the Oh My Pi transcript
   * entry `LEGION_FALLBACK_ENTRY`), on every change of it. */
  readonly saveState: (state: FallbackCooldownState) => void;
  /** Epoch ms. The cooldown machine reads no clock of its own, so every input carries this. */
  readonly now: () => number;
  readonly limits: FallbackCooldownLimits;
  /** `nextFallbackModel` (`@legion/pi-shared/fallback-chain`): the model below this one in the
   * deployment's own `retry.fallbackChains`. */
  readonly nextModel: (
    ctx: FallbackChainContext,
    model: HostModel
  ) => Promise<HostModel | undefined>;
  readonly log: FallbackChainLog;
}

export interface ModelFallback {
  /** `session_start`: the state the branch holds (`restoreFallbackCooldown`), or undefined when
   * this session has never fallen back. A relaunched worker comes back with its due time and its
   * doubling levels. */
  readonly restore: (state: FallbackCooldownState | undefined) => void;
  /** The host's `retry_fallback_applied`: the host walked the chain itself, and its own reason
   * says whether a content filter is what it walked for. */
  readonly armFromEvent: (event: RetryFallbackAppliedEvent, ctx: ModelFallbackContext) => void;
  /** The host's `agent_end`: a turn that has settled, which the provider ended cleanly, with a
   * content filter, or with some other failure. */
  readonly turnSettled: (event: AgentEndEvent, ctx: ModelFallbackContext) => Promise<void>;
  /** The host's `context`, which runs before every provider request: the arming's model goes back
   * on as soon as its wait is over, inside a long tool loop as well as at a new prompt. */
  readonly maybeRestore: (ctx: ModelFallbackContext) => Promise<void>;
}

/** The session's own fallback, as the claim check found it on the branch. */
interface OwnFallback {
  /** The `model_change` entry the host's walk wrote. */
  readonly entryId: string;
  /** The `model_change` before it, when the branch holds one. */
  readonly previousEntryId: string | undefined;
}

/** A `model_change` as `getBranch()` hands it back (`ModelChangeEntry`, `src/pi-types.ts`). The
 * branch is read as `unknown`, field by field, as `provider-refusal.ts` reads its message: a host
 * whose entry shape differs leaves the fallback unclaimed rather than throwing in a handler. */
function isModelChange(entry: unknown): entry is ModelChangeEntry {
  return (
    typeof entry === "object" &&
    entry !== null &&
    "type" in entry &&
    entry.type === "model_change" &&
    "id" in entry &&
    typeof entry.id === "string" &&
    entry.id !== ""
  );
}

/** The index of the newest `model_change` before `end`, or -1 when the branch holds none. */
function modelChangeBefore(branch: readonly unknown[], end: number): number {
  for (let index = Math.min(end, branch.length) - 1; index >= 0; index -= 1) {
    if (isModelChange(branch[index])) return index;
  }
  return -1;
}

function sameModel(a: HostModel | undefined, b: HostModel | undefined): boolean {
  return a !== undefined && b !== undefined && a.provider === b.provider && a.id === b.id;
}

function selectorOf(model: HostModel): string {
  return `${model.provider}/${model.id}`;
}

/** The run's last assistant message, which says how the provider ended the turn. */
function lastAssistantMessage(
  messages: readonly AgentEndMessage[] | undefined
): AgentEndMessage | undefined {
  if (messages === undefined) return undefined;
  for (let index = messages.length - 1; index >= 0; index -= 1) {
    const message = messages[index];
    if (message?.role === "assistant") return message;
  }
  return undefined;
}

export function createModelFallback(deps: ModelFallbackDeps): ModelFallback {
  let state: FallbackCooldownState = { levels: {} };
  /** Every `model_change` entry an event has already claimed, so a second event — an advisor's
   * that follows the session's own onto the same model — does not claim it again. */
  const claimed = new Set<string>();
  /** The entry of a fallback the host applied since the last settled turn, so a content filter
   * that settles after it is known to be one the host already walked. */
  let appliedThisTurn: string | undefined;

  /** Keeps the state and persists it, and only when the step changed it: every unchanged step of
   * the machine answers the state it was given. */
  const save = (next: FallbackCooldownState): void => {
    if (next === state) return;
    state = next;
    deps.saveState(next);
  };

  /** One step of the cooldown machine against the state this runner holds. */
  const step = (input: FallbackCooldownInput): FallbackCooldownState =>
    stepFallbackCooldown(state, input, deps.limits);

  /**
   * The event's own fallback, or undefined when it is not this session's.
   *
   * Ported from the reference extension (sjawhar/dotfiles `omp/extensions/fallback-event.ts`),
   * whose reasoning holds here. Oh My Pi emits `retry_fallback_applied` from two places: its turn
   * recovery, when it moves the session's own model down `retry.fallbackChains`, and its advisor
   * runtime, when an advisor's own model fails over. The payloads look the same, and an advisor's
   * step leaves the session's model alone. Before the first kind the host appends a
   * `model_change` naming the new model with `role: "fallback"` and `resolvedModelIsFallback:
   * true`; the advisor path appends nothing, and the host's own restore appends `role: "fallback"`
   * with `resolvedModelIsFallback: false`. So an event is the session's own when the newest
   * `model_change` on the branch is such an entry, resolves to the event's `to`, has not been
   * claimed by an earlier event, and follows a `model_change` naming the event's `from`. The last
   * two close two holes: an advisor event that follows a session fallback onto the same model
   * finds the entry claimed, and one that lands between the host appending the entry and emitting
   * the session's event names the wrong `from`.
   */
  const ownFallback = (
    event: RetryFallbackAppliedEvent,
    ctx: ModelFallbackContext
  ): OwnFallback | undefined => {
    const resolve = ctx.models?.resolve;
    if (resolve === undefined) return undefined;
    const branch = ctx.sessionManager.getBranch?.() ?? [];
    const at = modelChangeBefore(branch, branch.length);
    const entry = at < 0 ? undefined : branch[at];
    if (!isModelChange(entry)) return undefined;
    if (entry.model === undefined) return undefined;
    if (entry.role !== "fallback" || entry.resolvedModelIsFallback !== true) return undefined;
    if (claimed.has(entry.id)) return undefined;
    if (!sameModel(resolve(event.to), resolve(entry.model))) return undefined;
    const beforeAt = modelChangeBefore(branch, at);
    const before = beforeAt < 0 ? undefined : branch[beforeAt];
    const previous = isModelChange(before) ? before : undefined;
    const beforeModel = previous?.model === undefined ? undefined : resolve(previous.model);
    const fromModel = resolve(event.from);
    // Lenient when either side resolves to nothing: a missed fallback costs more than this race.
    if (beforeModel !== undefined && fromModel !== undefined && !sameModel(beforeModel, fromModel))
      return undefined;
    claimed.add(entry.id);
    return { entryId: entry.id, previousEntryId: previous?.id };
  };

  /**
   * The chain walk's context, or undefined when this host exposes no registry to walk with. The
   * host's own `ModelRegistry` methods read its private state, so each is called on the registry
   * rather than handed over detached.
   */
  const chainContext = (ctx: ModelFallbackContext): FallbackChainContext | undefined => {
    const registry = ctx.modelRegistry;
    const models = ctx.models;
    if (models === undefined) return undefined;
    if (registry?.find === undefined || registry.hasProvider === undefined) return undefined;
    return {
      modelRegistry: {
        find: (provider, id) => registry.find?.(provider, id),
        hasProvider: (provider) => registry.hasProvider?.(provider) === true,
      },
      models,
      log: deps.log,
    };
  };

  /**
   * The plugin's own step down the chain, for a content filter the host did not walk: the host
   * leaves a refusal that lands after the model has already sent text where it is
   * (`turn-recovery.ts:1625-1653` and `:2693-2703` at the pinned tag
   * `v18.16.0-sami.20261104-021732`).
   *
   * The arming names the `model_change` entry **this** call wrote, read back from `getBranch()`
   * after it. The host's own fallback claim would not do: `pi.setModel` clears the active retry
   * fallback and appends an ordinary `model_change` (`session/model-controls.ts:249-252` at the
   * pinned tag), so nothing on the branch would carry the host's claim at all.
   */
  const walkChain = async (ctx: ModelFallbackContext, current: HostModel): Promise<void> => {
    const selector = selectorOf(current);
    const chain = chainContext(ctx);
    if (chain === undefined) {
      deps.log.warn(
        "legion: a content filter refused this turn and this host exposes no model registry",
        {
          model: selector,
        }
      );
      return;
    }
    const next = await deps.nextModel(chain, current);
    if (next === undefined) {
      deps.log.warn("legion: a content filter refused this turn and the chain has no next model", {
        model: selector,
      });
      return;
    }
    if (!(await deps.setModel(next))) {
      deps.log.warn("legion: the next model of the chain could not be set; staying on this one", {
        model: selector,
        next: selectorOf(next),
      });
      return;
    }
    const branch = ctx.sessionManager.getBranch?.() ?? [];
    const at = modelChangeBefore(branch, branch.length);
    const entry = at < 0 ? undefined : branch[at];
    if (!isModelChange(entry)) {
      deps.log.warn("legion: the model switch left no model_change entry to arm the return on", {
        model: selector,
        next: selectorOf(next),
      });
      return;
    }
    save(
      step({
        kind: "fallback-applied",
        restoreTo: selector,
        entryId: entry.id,
        cause: "refusal",
        now: deps.now(),
      })
    );
  };

  const settled = async (event: AgentEndEvent, ctx: ModelFallbackContext): Promise<void> => {
    // The host is about to retry this turn itself, so it has not settled; acting on it would step
    // the session down twice (`session/agent-session.ts:5666-5674` at the pinned tag).
    if (event.willContinue === true) return;
    const applied = appliedThisTurn;
    appliedThisTurn = undefined;
    const outcome = classifyProviderTurn(lastAssistantMessage(event.messages));
    const current = ctx.models?.current?.();
    if (outcome.kind === "content-filter") {
      // The host walked the chain for this very refusal: the arming it left becomes a refusal's,
      // so it waits out the growing cooldown rather than the host's own suppression. A host
      // build whose event named the filter armed that already, and arming it twice would double
      // the selector's level for one walk, so this is the path for a build that named nothing.
      if (applied !== undefined && state.arming?.entryId === applied) {
        if (state.arming.cause === "refusal") return;
        save(
          step({
            kind: "fallback-applied",
            restoreTo: state.arming.restoreTo,
            entryId: state.arming.entryId,
            cause: "refusal",
            now: deps.now(),
          })
        );
        return;
      }
      if (current === undefined) {
        deps.log.warn("legion: a content filter refused this turn and this host names no model");
        return;
      }
      await walkChain(ctx, current);
      return;
    }
    if (current === undefined) return;
    save(
      step(
        outcome.kind === "provider-error"
          ? { kind: "provider-error-turn", selector: selectorOf(current) }
          : { kind: "clean-turn", selector: selectorOf(current) }
      )
    );
  };

  const restoreIfDue = async (ctx: ModelFallbackContext): Promise<void> => {
    const arming = state.arming;
    if (arming === undefined) return;
    const branch = ctx.sessionManager.getBranch?.() ?? [];
    const at = modelChangeBefore(branch, branch.length);
    const newest = at < 0 ? undefined : branch[at];
    // Any model change since the fallback — the operator's `/model`, the host's own restore,
    // another session resumed in this process — is not this arming's to undo.
    if (!isModelChange(newest) || newest.id !== arming.entryId) {
      save(step({ kind: "superseded" }));
      return;
    }
    const target = ctx.models?.resolve?.(arming.restoreTo);
    if (target === undefined) {
      // Gone from this session's roster (a credential lost, a catalog change): nothing to go back
      // to, so the arming ends rather than being tried at every request for the rest of the run.
      save(step({ kind: "superseded" }));
      return;
    }
    if (sameModel(ctx.models?.current?.(), target)) {
      save(step({ kind: "restored" }));
      return;
    }
    const suppressed = ctx.modelRegistry?.isSelectorSuppressed?.(arming.restoreTo) === true;
    if (restoreDue(state, deps.now(), suppressed) === undefined) return;
    if (!(await deps.setModel(target))) {
      deps.log.warn("legion: the model this session fell back from could not be set again", {
        model: arming.restoreTo,
      });
      return;
    }
    save(step({ kind: "restored" }));
  };

  /** What a handler does with a failure of its own: one warning, and the host's event is over. */
  const contained = (what: string, error: unknown): void => {
    deps.log.warn(`legion: the model fallback runner failed to ${what}`, {
      error: messageFor(error),
    });
  };

  return {
    restore: (restored) => {
      if (restored !== undefined) state = restored;
    },
    armFromEvent: (event, ctx) => {
      try {
        // The Fireworks Fast degrade keeps the model and drops only the tier that refused the
        // session, so restoring it would fail again every cooldown.
        if (event.role === "fireworks-fast") return;
        const own = ownFallback(event, ctx);
        if (own === undefined) return;
        appliedThisTurn = own.entryId;
        // A step that continues the same walk keeps the model the session started on rather than
        // the one it has just left; a fallback after any other model change starts a new arming.
        const previous = state.arming;
        const continues = previous !== undefined && own.previousEntryId === previous.entryId;
        // What the host walked for, in the provider's own words: it names the reason as
        // `Request failed: ` and the error text of the message that failed
        // (`session/turn-recovery.ts` at the pinned tag `v18.16.0-sami.20261104-021732`), so
        // `Request failed: Refusal (cyber): Declined` reads as Anthropic's `cyber` filter and
        // arms the same refusal the plugin's own walk arms, category and all.
        //
        // Reading it here is what covers a refusal that came before the model sent any text:
        // the host walks the chain itself and then replays the turn, which succeeds, so the
        // settled turn's last assistant message is clean and `settled` below never sees that
        // refusal. An `"other"` arming would then wait on a suppression the host never applies
        // — for a classifier refusal it pins the fallback instead
        // (`session/turn-recovery.ts:3229` at the pinned tag) — and hand the session straight
        // back to the model that had just refused it, with no cooldown at all.
        //
        // A host build that sends no reason arms `"other"`, as every arming did before: for it
        // `settled` stays the path that re-arms the fallback as a refusal once the turn ends on
        // the filter's own message. That is why both paths exist.
        const refusal =
          event.reason === undefined ? undefined : classifyProviderError(event.reason);
        save(
          step({
            kind: "fallback-applied",
            cause: refusal?.kind === "content-filter" ? "refusal" : "other",
            restoreTo: continues ? previous.restoreTo : event.from,
            entryId: own.entryId,
            now: deps.now(),
          })
        );
      } catch (error) {
        contained("arm the cooldown of a fallback the host applied", error);
      }
    },
    turnSettled: async (event, ctx) => {
      try {
        await settled(event, ctx);
      } catch (error) {
        contained("read how the provider ended this turn", error);
      }
    },
    maybeRestore: async (ctx) => {
      try {
        await restoreIfDue(ctx);
      } catch (error) {
        contained("put the session back on the model it fell back from", error);
      }
    },
  };
}
