import { integerSetting, type StallLimitsLog } from "./stall-limits";

/**
 * The cooldown a worker's model fallback runs on (INFRA-956 R1): how long a session stays on the
 * weaker model after a provider's content filter refused a turn, and when it goes back.
 *
 * Pure, in the shape of `phase-stall.ts`: it imports nothing of Oh My Pi and reads no clock, so
 * every input carries the time it happened at and a test names that time. The runner that drives
 * it owns the host's events and the model switch; this module decides nothing but the waits.
 *
 * A refusal waits out a cooldown that doubles each time the same model refuses again: one hour,
 * then two, four, and eight hours at most. A clean turn on a model puts its wait back to one hour;
 * a provider error that is no content filter leaves the wait as it is, because a rate limit is not
 * the model refusing. A fallback the host applied for any other reason carries no cooldown at all:
 * Oh My Pi's `retry_fallback_applied` names no reason, nothing reports when the host's own
 * suppression of a model expires, and the host restores such a fallback itself, so that arming
 * waits for the suppression to end (`modelRegistry.isSelectorSuppressed`, `src/pi-types.ts`) and
 * `restoreDue` is told whether it still holds.
 */

/** Why the session fell back: its turn was refused by a content filter, or anything else the host
 * steps down the chain for (a rate limit, an overload, a spent quota window). */
export type FallbackCause = "refusal" | "other";

export interface FallbackArming {
  /** The selector the fallback moved the session off (`provider/id`, as the host's
   * `retry_fallback_applied.from` writes it). */
  readonly restoreTo: string;
  /** The `model_change` entry this arming is tied to; a newer model change supersedes it. */
  readonly entryId: string;
  /** Epoch ms: the earliest time to put the session back. */
  readonly dueAt: number;
  /** Why the session fell back. A refusal waits out the cooldown; anything else waits for the
   * host's own suppression to end. */
  readonly cause: FallbackCause;
}

export interface FallbackCooldownState {
  readonly arming?: FallbackArming;
  /** How long the next refusal of each selector waits, as a doubling level (1 = the base wait). */
  readonly levels: Readonly<Record<string, number>>;
}

export type FallbackCooldownInput =
  /** The host moved the session off `restoreTo`, writing the `model_change` entry `entryId`. */
  | {
      readonly kind: "fallback-applied";
      readonly restoreTo: string;
      readonly entryId: string;
      readonly cause: FallbackCause;
      readonly now: number;
    }
  /** The session is back on the arming's `restoreTo`. */
  | { readonly kind: "restored" }
  /** A model change that is not this arming's: the operator's `/model`, the host's own restore,
   * another session. Nothing the fallback did is undone. */
  | { readonly kind: "superseded" }
  /** A turn on `selector` that ended with no provider error. */
  | { readonly kind: "clean-turn"; readonly selector: string }
  /** A turn on `selector` that ended in a provider error that was no content filter. */
  | { readonly kind: "provider-error-turn"; readonly selector: string };

/** How long a refusal of a selector at `level` waits: the base doubled `level - 1` times, at most
 * the cap. */
function waitMs(limits: FallbackCooldownLimits, level: number): number {
  return Math.min(limits.baseMs * 2 ** (level - 1), limits.maxMs);
}

/** The doubling level one selector stands at: 1 for a selector no refusal has reached yet, and for
 * a level a record of another build left unusable. */
function levelOf(state: FallbackCooldownState, selector: string): number {
  const level = state.levels[selector];
  return Number.isInteger(level) && level >= 1 ? level : 1;
}

/**
 * The state after one input.
 *
 * A refusal's arming is due `waitMs` from now and raises that selector's level, because the
 * **next** refusal of the same model must wait longer. The level stops climbing once its wait has
 * reached the cap: a higher level would buy nothing and could only overflow. Any other fallback is
 * armed due now — the wait is the host's own suppression, which `restoreDue` reads.
 */
export function stepFallbackCooldown(
  state: FallbackCooldownState,
  input: FallbackCooldownInput,
  limits: FallbackCooldownLimits
): FallbackCooldownState {
  switch (input.kind) {
    case "fallback-applied": {
      const { restoreTo, entryId, cause, now } = input;
      if (cause === "other") {
        return { ...state, arming: { restoreTo, entryId, dueAt: now, cause } };
      }
      const level = levelOf(state, restoreTo);
      const wait = waitMs(limits, level);
      return {
        arming: { restoreTo, entryId, dueAt: now + wait, cause },
        levels: { ...state.levels, [restoreTo]: wait < limits.maxMs ? level + 1 : level },
      };
    }
    case "restored":
    case "superseded":
      return { levels: state.levels };
    case "clean-turn":
      return levelOf(state, input.selector) === 1
        ? state
        : { ...state, levels: { ...state.levels, [input.selector]: 1 } };
    case "provider-error-turn":
      return state;
  }
}

/** The arming to act on now, or undefined: a refusal waits for its due time, any other fallback
 * waits for the host to stop suppressing the model. */
export function restoreDue(
  state: FallbackCooldownState,
  now: number,
  suppressed: boolean
): FallbackArming | undefined {
  const arming = state.arming;
  if (arming === undefined) return undefined;
  if (arming.cause === "refusal") return now >= arming.dueAt ? arming : undefined;
  return suppressed ? undefined : arming;
}

export interface FallbackCooldownLimits {
  /** How long the first refusal of a selector waits. */
  readonly baseMs: number;
  /** The longest wait the doubling reaches. */
  readonly maxMs: number;
}

/** The numbers a deployment that sets neither variable gets. */
export const DEFAULT_FALLBACK_COOLDOWN: FallbackCooldownLimits = {
  // 1 hour is the window Anthropic documents for its own post-refusal routing, and the window the
  // specification names.
  baseMs: 3_600_000,
  // 8 hours: a model that keeps refusing the same context is tried three times a day rather than
  // every hour, and a worker's context rarely outlives that.
  maxMs: 28_800_000,
};

/** The least either variable may be set to. A live proof sets them to a few seconds; anything
 * under a second is a typo, and a base of 0 would restore the session inside its own turn. */
const LEAST_COOLDOWN_MS = 1_000;

/**
 * The cooldown this worker runs on, read from its own environment as `stallLimits` reads its four
 * (`src/stall-limits.ts`). No variable is required: a deployment that sets neither gets one hour
 * and eight hours, and a live proof sets both to a few seconds.
 */
export function fallbackCooldownLimits(
  env: Record<string, string | undefined>,
  log: StallLimitsLog
): FallbackCooldownLimits {
  return {
    baseMs: integerSetting(
      env,
      log,
      "LEGION_REFUSAL_COOLDOWN_MS",
      DEFAULT_FALLBACK_COOLDOWN.baseMs,
      LEAST_COOLDOWN_MS
    ),
    maxMs: integerSetting(
      env,
      log,
      "LEGION_REFUSAL_COOLDOWN_MAX_MS",
      DEFAULT_FALLBACK_COOLDOWN.maxMs,
      LEAST_COOLDOWN_MS
    ),
  };
}

/** The custom transcript entry (`pi.appendEntry`) holding the state after each change, so a worker
 * the daemon relaunches with `--resume` restores its due time and its levels from the branch. */
export const LEGION_FALLBACK_ENTRY = "legion-model-fallback";

/** What one recorded entry carries: the state's own two fields, and nothing the machine keeps in
 * memory only. */
export interface RecordedFallbackCooldown {
  readonly arming?: FallbackArming;
  readonly levels: Readonly<Record<string, number>>;
}

/** The state as a record holds it, for the caller's `pi.appendEntry(LEGION_FALLBACK_ENTRY, …)`:
 * a state with no arming records none rather than a `null` field a reader would have to allow. */
export function recordedFallbackCooldown(state: FallbackCooldownState): RecordedFallbackCooldown {
  return state.arming === undefined
    ? { levels: state.levels }
    : { arming: state.arming, levels: state.levels };
}

/** The arming one record's `arming` field holds, or undefined when it is not one this build can
 * read. */
function armingFrom(value: unknown): FallbackArming | undefined {
  if (typeof value !== "object" || value === null) return undefined;
  const restoreTo = "restoreTo" in value ? value.restoreTo : undefined;
  const entryId = "entryId" in value ? value.entryId : undefined;
  const dueAt = "dueAt" in value ? value.dueAt : undefined;
  const cause = "cause" in value ? value.cause : undefined;
  if (typeof restoreTo !== "string" || restoreTo === "") return undefined;
  if (typeof entryId !== "string" || entryId === "") return undefined;
  if (typeof dueAt !== "number" || !Number.isFinite(dueAt)) return undefined;
  if (cause !== "refusal" && cause !== "other") return undefined;
  return { restoreTo, entryId, dueAt, cause };
}

/** The levels one record's `levels` field holds, or undefined when it holds anything that is not
 * a whole level of 1 or more. */
function levelsFrom(value: unknown): Record<string, number> | undefined {
  if (typeof value !== "object" || value === null) return undefined;
  const levels: Record<string, number> = {};
  for (const [selector, level] of Object.entries(value)) {
    if (!Number.isInteger(level) || level < 1) return undefined;
    levels[selector] = level;
  }
  return levels;
}

/** The state one entry's data holds, or undefined when it holds none this build understands: an
 * entry an older build wrote, or one with a field of the wrong type, is passed over rather than
 * read in part. */
function fallbackCooldownFrom(data: object): FallbackCooldownState | undefined {
  const levels = levelsFrom("levels" in data ? data.levels : undefined);
  if (levels === undefined) return undefined;
  const recorded = "arming" in data ? data.arming : undefined;
  if (recorded === undefined) return { levels };
  const arming = armingFrom(recorded);
  return arming === undefined ? undefined : { arming, levels };
}

/** The state the branch's newest readable fallback entry holds, or undefined when it holds none (a
 * session that has never fallen back). */
export function restoreFallbackCooldown(
  branch: readonly unknown[]
): FallbackCooldownState | undefined {
  for (let index = branch.length - 1; index >= 0; index -= 1) {
    const entry = branch[index];
    if (
      typeof entry === "object" &&
      entry !== null &&
      "type" in entry &&
      entry.type === "custom" &&
      "customType" in entry &&
      entry.customType === LEGION_FALLBACK_ENTRY &&
      "data" in entry &&
      typeof entry.data === "object" &&
      entry.data !== null
    ) {
      const state = fallbackCooldownFrom(entry.data);
      if (state !== undefined) return state;
    }
  }
  return undefined;
}
