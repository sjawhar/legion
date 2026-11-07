import { describe, expect, test } from "bun:test";
import {
  DEFAULT_FALLBACK_COOLDOWN,
  type FallbackCooldownState,
  fallbackCooldownLimits,
  LEGION_FALLBACK_ENTRY,
  recordedFallbackCooldown,
  restoreDue,
  restoreFallbackCooldown,
  stepFallbackCooldown,
} from "./fallback-cooldown";

const HOUR = 3_600_000;
const OPUS = "anthropic/claude-opus-5-5";
const GPT = "openai/gpt-5.5";
const EMPTY: FallbackCooldownState = { levels: {} };

/** The state after one refusal of `selector` at `now`, from `state`. */
function refused(state: FallbackCooldownState, selector: string, now: number, entryId = "e1") {
  return stepFallbackCooldown(
    state,
    { kind: "fallback-applied", restoreTo: selector, entryId, cause: "refusal", now },
    DEFAULT_FALLBACK_COOLDOWN
  );
}

describe("the wait a refusal earns", () => {
  test("the first refusal of a model waits an hour", () => {
    const state = refused(EMPTY, OPUS, 1_000);
    expect(state.arming).toEqual({
      restoreTo: OPUS,
      entryId: "e1",
      dueAt: 1_000 + HOUR,
      cause: "refusal",
    });
    expect(state.levels).toEqual({ [OPUS]: 2 });
  });

  test("the second and third refusals of the same model wait two and four hours", () => {
    const second = refused(refused(EMPTY, OPUS, 0), OPUS, 10_000);
    expect(second.arming?.dueAt).toBe(10_000 + 2 * HOUR);
    expect(second.levels).toEqual({ [OPUS]: 3 });

    const third = refused(second, OPUS, 20_000);
    expect(third.arming?.dueAt).toBe(20_000 + 4 * HOUR);
    expect(third.levels).toEqual({ [OPUS]: 4 });
  });

  test("the wait stops at eight hours, and the level stops with it", () => {
    let state = EMPTY;
    for (let refusal = 0; refusal < 6; refusal += 1) state = refused(state, OPUS, 0);
    expect(state.arming?.dueAt).toBe(8 * HOUR);
    expect(state.levels).toEqual({ [OPUS]: 4 });
    expect(refused(state, OPUS, 100).arming?.dueAt).toBe(100 + 8 * HOUR);
  });

  test("a clean turn on the restored model puts its wait back to an hour", () => {
    const climbed = refused(refused(EMPTY, OPUS, 0), OPUS, 0);
    const clean = stepFallbackCooldown(
      climbed,
      { kind: "clean-turn", selector: OPUS },
      DEFAULT_FALLBACK_COOLDOWN
    );
    expect(clean.levels).toEqual({ [OPUS]: 1 });
    expect(clean.arming).toEqual(climbed.arming);
    expect(refused(clean, OPUS, 500).arming?.dueAt).toBe(500 + HOUR);
  });

  test("two models keep their own levels", () => {
    const state = refused(refused(refused(EMPTY, OPUS, 0), OPUS, 0), GPT, 0);
    expect(state.levels).toEqual({ [OPUS]: 3, [GPT]: 2 });
    expect(state.arming?.dueAt).toBe(HOUR);
    expect(refused(state, OPUS, 0).arming?.dueAt).toBe(4 * HOUR);
  });
});

describe("when the session goes back", () => {
  /** The host's own suppression, as the runner reads it (`isSelectorSuppressed`). */
  const held = () => true;
  const free = () => false;

  test("a refusal's arming is due only at its due time, and asks the host nothing", () => {
    const state = refused(EMPTY, OPUS, 1_000);
    let asked = 0;
    const asking = () => {
      asked += 1;
      return false;
    };
    expect(restoreDue(state, 1_000 + HOUR - 1, asking)).toBeUndefined();
    expect(restoreDue(state, 1_000 + HOUR, asking)).toEqual(state.arming);
    // A refusal waits out its own cooldown, whatever the host holds suppressed.
    expect(restoreDue(state, 1_000 + HOUR, held)).toEqual(state.arming);
    expect(asked).toBe(0);
  });

  test("any other fallback waits for the host to stop suppressing the model", () => {
    const state = stepFallbackCooldown(
      EMPTY,
      { kind: "fallback-applied", restoreTo: OPUS, entryId: "e9", cause: "other", now: 5_000 },
      DEFAULT_FALLBACK_COOLDOWN
    );
    expect(state.arming).toEqual({
      restoreTo: OPUS,
      entryId: "e9",
      dueAt: 5_000,
      cause: "other",
    });
    expect(state.levels).toEqual({});
    expect(restoreDue(state, 6_000, held)).toBeUndefined();
    expect(restoreDue(state, 6_000, free)).toEqual(state.arming);
  });

  test("a restored session keeps its levels and is due nothing", () => {
    const state = stepFallbackCooldown(
      refused(EMPTY, OPUS, 0),
      { kind: "restored" },
      DEFAULT_FALLBACK_COOLDOWN
    );
    expect(state).toEqual({ levels: { [OPUS]: 2 } });
    expect(restoreDue(state, 9 * HOUR, free)).toBeUndefined();
  });

  test("a superseded arming is dropped and nothing is undone", () => {
    const state = stepFallbackCooldown(
      refused(EMPTY, OPUS, 0),
      { kind: "superseded" },
      DEFAULT_FALLBACK_COOLDOWN
    );
    expect(state).toEqual({ levels: { [OPUS]: 2 } });
    expect(restoreDue(state, 9 * HOUR, free)).toBeUndefined();
  });
});

describe("the limits the worker's environment sets", () => {
  const silent = { warn: () => {} };

  test("a deployment that sets neither variable gets one hour and eight hours", () => {
    expect(fallbackCooldownLimits({}, silent)).toEqual({
      baseMs: HOUR,
      maxMs: 8 * HOUR,
    });
    expect(DEFAULT_FALLBACK_COOLDOWN).toEqual({ baseMs: HOUR, maxMs: 8 * HOUR });
  });

  test("a live proof sets both to a few seconds", () => {
    expect(
      fallbackCooldownLimits(
        { LEGION_REFUSAL_COOLDOWN_MS: "2000", LEGION_REFUSAL_COOLDOWN_MAX_MS: "8000" },
        silent
      )
    ).toEqual({ baseMs: 2_000, maxMs: 8_000 });
  });

  test("a value that is no whole number of a second or more warns and leaves the default", () => {
    const warnings: string[] = [];
    const limits = fallbackCooldownLimits(
      { LEGION_REFUSAL_COOLDOWN_MS: "250", LEGION_REFUSAL_COOLDOWN_MAX_MS: "soon" },
      { warn: (message) => warnings.push(message) }
    );
    expect(limits).toEqual(DEFAULT_FALLBACK_COOLDOWN);
    expect(warnings).toEqual([
      "legion: LEGION_REFUSAL_COOLDOWN_MS is not an integer of 1000 or more; using 3600000",
      "legion: LEGION_REFUSAL_COOLDOWN_MAX_MS is not an integer of 1000 or more; using 28800000",
    ]);
  });
});

describe("what a relaunch restores", () => {
  /** The branch entry `pi.appendEntry(LEGION_FALLBACK_ENTRY, …)` writes. */
  function entry(data: unknown): unknown {
    return { type: "custom", customType: LEGION_FALLBACK_ENTRY, data };
  }

  test("an arming whose due time has already passed comes back due", () => {
    const state = refused(EMPTY, OPUS, 1_000);
    const restored = restoreFallbackCooldown([
      { type: "user" },
      entry(recordedFallbackCooldown(state)),
    ]);
    expect(restored).toEqual(state);
    expect(restoreDue(restored ?? EMPTY, 2_000 + 2 * HOUR, () => false)).toEqual(state.arming);
  });

  test("a state with no arming records none and reads back with its levels", () => {
    const state = stepFallbackCooldown(
      refused(EMPTY, OPUS, 0),
      { kind: "restored" },
      DEFAULT_FALLBACK_COOLDOWN
    );
    const record = recordedFallbackCooldown(state);
    expect(record).toEqual({ levels: { [OPUS]: 2 } });
    expect("arming" in record).toBe(false);
    expect(restoreFallbackCooldown([entry(record)])).toEqual(state);
  });

  test("the newest readable entry wins, and one of the wrong shape is passed over", () => {
    const good = recordedFallbackCooldown(refused(EMPTY, OPUS, 1_000));
    expect(
      restoreFallbackCooldown([
        entry(good),
        entry({ levels: { [OPUS]: 2 }, arming: { restoreTo: OPUS, entryId: "e1", dueAt: "soon" } }),
        entry({ state: "open", count: 2 }),
        entry({ levels: { [OPUS]: "two" } }),
        entry("not an object"),
      ])
    ).toEqual({
      arming: { restoreTo: OPUS, entryId: "e1", dueAt: 1_000 + HOUR, cause: "refusal" },
      levels: { [OPUS]: 2 },
    });
  });

  test("a branch with no entry of this type restores nothing", () => {
    expect(restoreFallbackCooldown([])).toBeUndefined();
    expect(
      restoreFallbackCooldown([{ type: "custom", customType: "legion-phase-stall", data: {} }])
    ).toBeUndefined();
  });
});
