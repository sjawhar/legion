import { beforeEach, describe, expect, test } from "bun:test";
import type { FallbackCooldownState } from "@legion/pi-shared/fallback-cooldown";
import type { HostModel } from "@legion/pi-shared/pi-types";
import { createModelFallback, type ModelFallbackContext } from "./model-fallback";

/**
 * The model fallback runner against fake deps and a fake session context: no Oh My Pi, no pane.
 * The host's own behaviour the runner leans on is written into the fixture — `pi.setModel`
 * appends an ordinary `model_change` and the host's walk appends a fallback claim — so a test
 * reads as the sequence the pane would see.
 */

const OPUS = "anthropic/claude-opus-5-5";
const SONNET = "anthropic/claude-sonnet-5";
const GPT = "openai/gpt-5.5";
const HOUR = 3_600_000;

function model(selector: string): HostModel {
  const [provider, id] = selector.split("/");
  return { provider: provider ?? "", id: id ?? "" };
}

/** A turn the provider ended with Anthropic's own refusal text. */
const REFUSED = {
  messages: [
    { role: "assistant", stopReason: "error", errorMessage: "Refusal (reasoning_extraction): no" },
  ],
} as const;
const CLEAN = { messages: [{ role: "assistant", stopReason: "stop" }] } as const;
const OVERLOADED = {
  messages: [{ role: "assistant", stopReason: "error", errorMessage: "529 Overloaded" }],
} as const;

let entries = 0;

function fixture(startOn = OPUS) {
  const branch: unknown[] = [];
  const setModelCalls: string[] = [];
  const saved: FallbackCooldownState[] = [];
  const warnings: string[] = [];
  /** The model below each selector in this deployment's chain. */
  const chain = new Map<string, string>();
  /** Selectors the host itself holds suppressed. */
  const suppressed = new Set<string>();
  /** Selectors this session can use no model for. */
  const unresolvable = new Set<string>();
  const live = { now: 1_000_000_000, current: startOn, setModelAnswers: true };

  /** One `model_change` as the host appends it, and its id. Every entry carries the time it was
   * written, as the host stamps them all (`SessionEntryBase`, `session/session-entries.ts` at
   * the pinned tag). */
  const append = (to: string, role: string, fallback: boolean): string => {
    entries += 1;
    const id = `entry-${entries}`;
    branch.push({
      type: "model_change",
      id,
      timestamp: new Date(live.now).toISOString(),
      model: to,
      role,
      resolvedModelIsFallback: fallback,
    });
    live.current = to;
    return id;
  };

  const ctx: ModelFallbackContext = {
    sessionManager: { getBranch: () => branch },
    models: {
      list: () => [],
      current: () => model(live.current),
      resolve: (spec) => (unresolvable.has(spec) ? undefined : model(spec)),
    },
    modelRegistry: {
      // The chain walk itself is the `nextModel` dep here, so the lookup is never consulted; it
      // is present because the runner refuses to walk a host that exposes no registry.
      find: () => undefined,
      hasProvider: () => true,
      isSelectorSuppressed: (selector) => suppressed.has(selector),
    },
  };

  const runner = createModelFallback({
    setModel: async (next) => {
      setModelCalls.push(`${next.provider}/${next.id}`);
      if (!live.setModelAnswers) return false;
      // `pi.setModel` clears the active retry fallback and appends an ordinary `model_change`
      // (`session/model-controls.ts:249-252` at the pinned tag), never a fallback claim.
      append(`${next.provider}/${next.id}`, "default", false);
      return true;
    },
    saveState: (state) => void saved.push(state),
    now: () => live.now,
    limits: { baseMs: HOUR, maxMs: 8 * HOUR },
    nextModel: async (_chain, current) => {
      const next = chain.get(`${current.provider}/${current.id}`);
      return next === undefined ? undefined : model(next);
    },
    log: { warn: (message) => void warnings.push(message) },
  });

  return {
    branch,
    setModelCalls,
    saved,
    warnings,
    chain,
    suppressed,
    unresolvable,
    live,
    ctx,
    runner,
    /** The `model_change` the host's own fallback walk appends before it emits its event. */
    hostFallback: (to: string) => append(to, "fallback", true),
    /** Any other model change: the operator's `/model`, the host's own restore. */
    modelChange: (to: string) => append(to, "default", false),
    /** The state the last save recorded. */
    last: () => saved[saved.length - 1],
  };
}

/** The reason the host names on the event when a content filter refused the turn it walked for:
 * what the live pane proof measured (dispatch://INFRA-956 R1). */
const REFUSAL_REASON = "Request failed: Refusal (cyber): Declined";
/** A reason that is no content filter: the busy model the host's own retry handles. */
const BUSY_REASON = "Request failed: 529 Overloaded";

/** The `retry_fallback_applied` payload the host emits for a session's own walk. `reason` is the
 * host's own wording of the error it walked for; a build that names none sends the event with no
 * such field at all. */
function applied(from: string, to: string, role = "default", reason?: string) {
  return { from, to, role, ...(reason === undefined ? {} : { reason }) };
}

beforeEach(() => {
  entries = 0;
});

describe("a fallback the host applied", () => {
  test("names no reason, so it is armed without a cooldown until the turn's end calls it one", () => {
    const pane = fixture();
    const entry = pane.hostFallback(SONNET);

    pane.runner.armFromEvent(applied(OPUS, SONNET), pane.ctx);

    expect(pane.last()?.arming).toEqual({
      restoreTo: OPUS,
      entryId: entry,
      dueAt: pane.live.now,
      cause: "other",
    });
    // Nothing is refused yet, so no selector's wait has doubled.
    expect(pane.last()?.levels).toEqual({});
  });

  test("whose reason names a content filter is armed as a refusal, which waits out its wait", async () => {
    const pane = fixture();
    const entry = pane.hostFallback(SONNET);

    pane.runner.armFromEvent(applied(OPUS, SONNET, "default", REFUSAL_REASON), pane.ctx);

    expect(pane.last()?.arming).toEqual({
      restoreTo: OPUS,
      entryId: entry,
      dueAt: pane.live.now + HOUR,
      cause: "refusal",
    });
    // The level rises as it does for the plugin's own walk: the next refusal waits twice as long.
    expect(pane.last()?.levels).toEqual({ [OPUS]: 2 });

    // The host's own replay of that turn succeeded, so the settled turn reads clean and the
    // arming the event made is the whole of it.
    const saves = pane.saved.length;
    await pane.runner.turnSettled(CLEAN, pane.ctx);
    expect(pane.saved.length).toBe(saves);

    // The host suppresses nothing for a refusal it walked for, so an `other` arming would be
    // handed back at the very next request. This one goes back only once its wait is over.
    await pane.runner.maybeRestore(pane.ctx);
    expect(pane.setModelCalls).toEqual([]);
    pane.live.now += HOUR;
    await pane.runner.maybeRestore(pane.ctx);
    expect(pane.setModelCalls).toEqual([OPUS]);
  });

  // The reviewer's own case (dispatch://INFRA-956 R1, round 1): `opus-5-5` refused before any
  // text, which the host walked itself, and then `opus-5` refused after text, which the host
  // leaves where it is. Both refusals land in one turn, and the session must end up one step
  // further down the chain with its return still pointing at the model it started on.
  test("is stepped down again when the fallback model refuses too, and still returns to the model the session started on", async () => {
    const pane = fixture();
    pane.chain.set(SONNET, GPT);
    pane.hostFallback(SONNET);
    pane.runner.armFromEvent(applied(OPUS, SONNET, "default", REFUSAL_REASON), pane.ctx);

    // The host's replay of the turn was refused too, after the model had sent text: a second
    // refusal, of the model the host had just moved the session to, which no fallback was
    // applied for.
    await pane.runner.turnSettled(REFUSED, pane.ctx);

    expect(pane.setModelCalls).toEqual([GPT]);
    expect(pane.last()?.arming).toEqual({
      restoreTo: OPUS,
      entryId: `entry-${entries}`,
      dueAt: pane.live.now + 2 * HOUR,
      cause: "refusal",
    });
    // The walk continued the host's own, so the level that rose is the one of the model the
    // session comes back to, and its wait had already doubled once.
    expect(pane.last()?.levels).toEqual({ [OPUS]: 3 });

    pane.live.now += 2 * HOUR;
    await pane.runner.maybeRestore(pane.ctx);
    expect(pane.setModelCalls).toEqual([GPT, OPUS]);
  });

  test("whose reason names no content filter is armed as any other fallback is", () => {
    const pane = fixture();
    const entry = pane.hostFallback(SONNET);

    pane.runner.armFromEvent(applied(OPUS, SONNET, "default", BUSY_REASON), pane.ctx);

    expect(pane.last()?.arming).toEqual({
      restoreTo: OPUS,
      entryId: entry,
      dueAt: pane.live.now,
      cause: "other",
    });
    expect(pane.last()?.levels).toEqual({});
  });

  test("whose reason names a busy model is stepped down when a filter then ends the turn", async () => {
    const pane = fixture();
    pane.chain.set(SONNET, GPT);
    pane.hostFallback(SONNET);
    pane.runner.armFromEvent(applied(OPUS, SONNET, "default", BUSY_REASON), pane.ctx);

    // The host walked for the overload, not for a filter, so the filter that ends this turn is
    // the fallback model's own refusal and no fallback was applied for it.
    await pane.runner.turnSettled(REFUSED, pane.ctx);

    expect(pane.setModelCalls).toEqual([GPT]);
    expect(pane.last()?.arming).toEqual({
      restoreTo: OPUS,
      entryId: `entry-${entries}`,
      dueAt: pane.live.now + HOUR,
      cause: "refusal",
    });
    expect(pane.last()?.levels).toEqual({ [OPUS]: 2 });
  });

  test("becomes a refusal's when the turn settles on a content filter, and is not walked twice", async () => {
    const pane = fixture();
    const entry = pane.hostFallback(SONNET);
    pane.chain.set(SONNET, GPT);
    pane.runner.armFromEvent(applied(OPUS, SONNET), pane.ctx);

    await pane.runner.turnSettled(REFUSED, pane.ctx);

    // The host walked the chain for this refusal, so the plugin only re-reads the arming.
    expect(pane.setModelCalls).toEqual([]);
    expect(pane.last()?.arming).toEqual({
      restoreTo: OPUS,
      entryId: entry,
      dueAt: pane.live.now + HOUR,
      cause: "refusal",
    });
    // The next refusal of that model waits twice as long.
    expect(pane.last()?.levels).toEqual({ [OPUS]: 2 });
  });

  test("keeps the model the session started on when a second step continues the same walk", () => {
    const pane = fixture();
    pane.hostFallback(SONNET);
    pane.runner.armFromEvent(applied(OPUS, SONNET), pane.ctx);
    const second = pane.hostFallback(GPT);

    pane.runner.armFromEvent(applied(SONNET, GPT), pane.ctx);

    expect(pane.last()?.arming).toEqual({
      restoreTo: OPUS,
      entryId: second,
      dueAt: pane.live.now,
      cause: "other",
    });
  });

  test("is dropped for the Fireworks Fast degrade and for an advisor's own fallback", () => {
    const pane = fixture();
    pane.hostFallback(SONNET);

    // The host keeps the model and drops only the tier, so restoring it would fail again.
    pane.runner.armFromEvent(applied(OPUS, SONNET, "fireworks-fast"), pane.ctx);
    expect(pane.saved).toEqual([]);

    // An advisor's failover appends no entry of its own, so the newest model change is the one
    // the session's own walk already claimed.
    pane.runner.armFromEvent(applied(OPUS, SONNET), pane.ctx);
    expect(pane.saved.length).toBe(1);
    pane.runner.armFromEvent(applied(GPT, SONNET), pane.ctx);
    expect(pane.saved.length).toBe(1);
  });
});

describe("a refusal the host did not walk", () => {
  test("steps down the chain and arms the return on the entry that switch wrote", async () => {
    const pane = fixture();
    pane.chain.set(OPUS, SONNET);

    await pane.runner.turnSettled(REFUSED, pane.ctx);

    expect(pane.setModelCalls).toEqual([SONNET]);
    expect(pane.last()?.arming).toEqual({
      restoreTo: OPUS,
      entryId: `entry-${entries}`,
      dueAt: pane.live.now + HOUR,
      cause: "refusal",
    });
    expect(pane.last()?.levels).toEqual({ [OPUS]: 2 });
  });

  test("walked a second time keeps the model the session started on and raises that model's wait", async () => {
    const pane = fixture();
    pane.chain.set(OPUS, SONNET);
    pane.chain.set(SONNET, GPT);
    await pane.runner.turnSettled(REFUSED, pane.ctx);

    // The next turn, on the model the first walk moved the session to, is refused as well.
    await pane.runner.turnSettled(REFUSED, pane.ctx);

    expect(pane.setModelCalls).toEqual([SONNET, GPT]);
    expect(pane.last()?.arming).toEqual({
      restoreTo: OPUS,
      entryId: `entry-${entries}`,
      dueAt: pane.live.now + 2 * HOUR,
      cause: "refusal",
    });
    expect(pane.last()?.levels).toEqual({ [OPUS]: 3 });
  });

  test("walked after another model change arms a fresh return on the model it leaves", async () => {
    const pane = fixture();
    pane.chain.set(OPUS, SONNET);
    pane.chain.set(GPT, SONNET);
    await pane.runner.turnSettled(REFUSED, pane.ctx);
    // The operator's own `/model` onto a third model, which is then refused: the walk that
    // follows continues no walk of this plugin's, so it arms on the model it leaves.
    pane.modelChange(GPT);

    await pane.runner.turnSettled(REFUSED, pane.ctx);

    expect(pane.setModelCalls).toEqual([SONNET, SONNET]);
    expect(pane.last()?.arming).toEqual({
      restoreTo: GPT,
      entryId: `entry-${entries}`,
      dueAt: pane.live.now + HOUR,
      cause: "refusal",
    });
    expect(pane.last()?.levels).toEqual({ [OPUS]: 2, [GPT]: 2 });
  });

  test("leaves the session where it is when the chain names no next model", async () => {
    const pane = fixture();

    await pane.runner.turnSettled(REFUSED, pane.ctx);

    expect(pane.setModelCalls).toEqual([]);
    expect(pane.saved).toEqual([]);
    expect(pane.warnings).toEqual([
      "legion: a content filter refused this turn and the chain has no next model",
    ]);
  });

  test("arms nothing when the host refuses the switch", async () => {
    const pane = fixture();
    pane.chain.set(OPUS, SONNET);
    pane.live.setModelAnswers = false;

    await pane.runner.turnSettled(REFUSED, pane.ctx);

    expect(pane.setModelCalls).toEqual([SONNET]);
    expect(pane.saved).toEqual([]);
    expect(pane.warnings).toEqual([
      "legion: the next model of the chain could not be set; staying on this one",
    ]);
  });

  test("is left alone while the host is about to retry the turn itself", async () => {
    const pane = fixture();
    pane.chain.set(OPUS, SONNET);

    await pane.runner.turnSettled({ ...REFUSED, willContinue: true }, pane.ctx);

    expect(pane.setModelCalls).toEqual([]);
    expect(pane.saved).toEqual([]);
  });
});

describe("a turn that no content filter ended", () => {
  test("puts a clean turn's model back to the base wait", async () => {
    const pane = fixture();
    pane.chain.set(OPUS, SONNET);
    await pane.runner.turnSettled(REFUSED, pane.ctx);
    pane.modelChange(OPUS);

    await pane.runner.turnSettled(CLEAN, pane.ctx);

    expect(pane.last()?.levels).toEqual({ [OPUS]: 1 });
  });

  test("leaves the level alone after a provider error that is no content filter", async () => {
    const pane = fixture();
    pane.chain.set(OPUS, SONNET);
    await pane.runner.turnSettled(REFUSED, pane.ctx);
    pane.modelChange(OPUS);
    const saves = pane.saved.length;

    await pane.runner.turnSettled(OVERLOADED, pane.ctx);

    expect(pane.saved.length).toBe(saves);
    expect(pane.last()?.levels).toEqual({ [OPUS]: 2 });
  });
});

describe("the return to the session's own model", () => {
  test("waits out a refusal's cooldown and then switches back", async () => {
    const pane = fixture();
    pane.chain.set(OPUS, SONNET);
    await pane.runner.turnSettled(REFUSED, pane.ctx);

    pane.live.now += HOUR - 1;
    await pane.runner.maybeRestore(pane.ctx);
    expect(pane.setModelCalls).toEqual([SONNET]);

    pane.live.now += 1;
    await pane.runner.maybeRestore(pane.ctx);
    expect(pane.setModelCalls).toEqual([SONNET, OPUS]);
    expect(pane.last()?.arming).toBeUndefined();
    expect(pane.last()?.levels).toEqual({ [OPUS]: 2 });
  });

  test("waits for the host's own suppression to end when the fallback was no refusal", async () => {
    const pane = fixture();
    pane.hostFallback(SONNET);
    pane.runner.armFromEvent(applied(OPUS, SONNET), pane.ctx);
    pane.suppressed.add(OPUS);

    await pane.runner.maybeRestore(pane.ctx);
    expect(pane.setModelCalls).toEqual([]);

    pane.suppressed.delete(OPUS);
    await pane.runner.maybeRestore(pane.ctx);
    expect(pane.setModelCalls).toEqual([OPUS]);
    expect(pane.last()?.arming).toBeUndefined();
  });

  test("is dropped when another model change superseded the fallback", async () => {
    const pane = fixture();
    pane.hostFallback(SONNET);
    pane.runner.armFromEvent(applied(OPUS, SONNET), pane.ctx);
    // The operator's own `/model`, the host's restore, another session.
    pane.modelChange(GPT);

    await pane.runner.maybeRestore(pane.ctx);

    expect(pane.setModelCalls).toEqual([]);
    expect(pane.last()?.arming).toBeUndefined();
  });

  test("is dropped when the model it would go back to no longer resolves", async () => {
    const pane = fixture();
    pane.hostFallback(SONNET);
    pane.runner.armFromEvent(applied(OPUS, SONNET), pane.ctx);
    pane.unresolvable.add(OPUS);

    await pane.runner.maybeRestore(pane.ctx);

    expect(pane.setModelCalls).toEqual([]);
    expect(pane.last()?.arming).toBeUndefined();
  });

  test("comes back from the transcript with its due time and its doubling level", async () => {
    const pane = fixture(SONNET);
    const entry = pane.hostFallback(SONNET);
    pane.runner.restore(
      {
        arming: {
          restoreTo: OPUS,
          entryId: entry,
          dueAt: pane.live.now + 1_000,
          cause: "refusal",
        },
        levels: { [OPUS]: 3 },
      },
      pane.ctx
    );

    await pane.runner.maybeRestore(pane.ctx);
    expect(pane.setModelCalls).toEqual([]);

    pane.live.now += 1_000;
    await pane.runner.maybeRestore(pane.ctx);
    expect(pane.setModelCalls).toEqual([OPUS]);

    // The level the record carried is the one the next refusal of that model waits on: the base
    // doubled twice, and the level climbs again.
    pane.chain.set(OPUS, SONNET);
    await pane.runner.turnSettled(REFUSED, pane.ctx);
    expect(pane.last()?.arming?.dueAt).toBe(pane.live.now + 4 * HOUR);
    expect(pane.last()?.levels).toEqual({ [OPUS]: 4 });
  });
});

// Plan answer 5 (dispatch://INFRA-956 R1): a session already sitting on a fallback when this
// build starts — one applied before the plugin shipped, or a host walk `ownFallback` did not
// claim — has no record of its own to restore, and nothing else would ever bring it back.
describe("a session this runner finds already on a fallback", () => {
  test("is armed from that entry's own time, on the model the change before it names", async () => {
    const pane = fixture();
    // The branch of a session the host walked down the chain: the model it ran on, then the
    // fallback claim the walk wrote.
    pane.modelChange(OPUS);
    const entry = pane.hostFallback(SONNET);
    const walkedAt = pane.live.now;
    // The relaunch: half an hour after the walk, with nothing of this plugin's in the branch.
    pane.live.now += HOUR / 2;

    pane.runner.restore(undefined, pane.ctx);

    expect(pane.last()?.arming).toEqual({
      restoreTo: OPUS,
      entryId: entry,
      dueAt: walkedAt + HOUR,
      cause: "refusal",
    });
    expect(pane.last()?.levels).toEqual({ [OPUS]: 2 });

    // The rest of the wait runs from the walk, not from the restore.
    await pane.runner.maybeRestore(pane.ctx);
    expect(pane.setModelCalls).toEqual([]);
    pane.live.now += HOUR / 2;
    await pane.runner.maybeRestore(pane.ctx);
    expect(pane.setModelCalls).toEqual([OPUS]);
  });

  // The reviewer's round-4 case (dispatch://INFRA-956 R4): two steps of one walk, neither of them
  // this plugin's, so the branch is the only record of where that walk started. The middle of the
  // chain is a model the session was handed by a refusal, and coming back to it is coming back to
  // no model of the role's.
  test("two steps down one walk comes back to the model that walk started on", async () => {
    const pane = fixture();
    pane.modelChange(OPUS);
    pane.hostFallback(SONNET);
    const entry = pane.hostFallback(GPT);
    const walkedAt = pane.live.now;

    pane.runner.restore(undefined, pane.ctx);

    expect(pane.last()?.arming).toEqual({
      restoreTo: OPUS,
      entryId: entry,
      dueAt: walkedAt + HOUR,
      cause: "refusal",
    });
    expect(pane.last()?.levels).toEqual({ [OPUS]: 2 });

    pane.live.now += HOUR;
    await pane.runner.maybeRestore(pane.ctx);
    expect(pane.setModelCalls).toEqual([OPUS]);
  });

  test("keeps the levels a record carried and arms on top of them", () => {
    const pane = fixture();
    pane.modelChange(OPUS);
    const entry = pane.hostFallback(SONNET);

    pane.runner.restore({ levels: { [OPUS]: 3 } }, pane.ctx);

    expect(pane.last()?.arming).toEqual({
      restoreTo: OPUS,
      entryId: entry,
      dueAt: pane.live.now + 4 * HOUR,
      cause: "refusal",
    });
    expect(pane.last()?.levels).toEqual({ [OPUS]: 4 });
  });

  test("whose entry carries no readable time waits the base cooldown from the restore", () => {
    const pane = fixture();
    pane.modelChange(OPUS);
    // An entry a build that stamped none wrote, or one whose stamp is no date.
    pane.branch.push({
      type: "model_change",
      id: "entry-unstamped",
      model: SONNET,
      role: "fallback",
      resolvedModelIsFallback: true,
    });

    pane.runner.restore(undefined, pane.ctx);

    expect(pane.last()?.arming).toEqual({
      restoreTo: OPUS,
      entryId: "entry-unstamped",
      dueAt: pane.live.now + HOUR,
      cause: "refusal",
    });
  });

  test("is left alone when the record already holds an arming, and when no fallback is live", () => {
    const armed = fixture();
    armed.modelChange(OPUS);
    const entry = armed.hostFallback(SONNET);
    const own = {
      arming: { restoreTo: GPT, entryId: entry, dueAt: armed.live.now + 5, cause: "refusal" },
      levels: {},
    } as const;
    armed.runner.restore(own, armed.ctx);
    expect(armed.saved).toEqual([]);

    // A session whose newest model change is the operator's own, or the host's own restore, is
    // on nobody's fallback.
    const ordinary = fixture();
    ordinary.modelChange(OPUS);
    ordinary.modelChange(SONNET);
    ordinary.runner.restore(undefined, ordinary.ctx);
    expect(ordinary.saved).toEqual([]);
  });

  test("names the model it cannot come back to when the branch records no earlier change", () => {
    const pane = fixture();
    pane.hostFallback(SONNET);

    pane.runner.restore(undefined, pane.ctx);

    expect(pane.saved).toEqual([]);
    expect(pane.warnings).toEqual([
      "legion: this session runs on a fallback model and its branch does not say which model it came from",
    ]);
  });

  test("takes the new session's own record when the pane is left on another session", async () => {
    const pane = fixture(SONNET);
    const entry = pane.hostFallback(SONNET);
    pane.runner.restore(
      {
        arming: { restoreTo: OPUS, entryId: entry, dueAt: pane.live.now, cause: "refusal" },
        levels: { [OPUS]: 2 },
      },
      pane.ctx
    );

    // The pane is switched to a session that never fell back: nothing of the old session's is
    // armed on it, and the model it runs on is its own business.
    const switched = fixture();
    switched.modelChange(OPUS);
    pane.runner.restore(undefined, switched.ctx);

    await pane.runner.maybeRestore(switched.ctx);
    expect(switched.setModelCalls).toEqual([]);
    expect(pane.setModelCalls).toEqual([]);
  });
});
