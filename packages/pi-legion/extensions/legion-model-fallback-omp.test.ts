import { afterEach, expect, test } from "bun:test";
import {
  type FallbackCooldownState,
  LEGION_FALLBACK_ENTRY,
} from "@legion/pi-shared/fallback-cooldown";
import {
  type Block,
  type Cleanup,
  call,
  type LegionPane,
  messageStream,
  type Refusal,
  type Request,
  refusalStream,
  runLegionPane,
  type StandinProvider,
} from "@legion/pi-shared/test/omp-harness";

// The model fallback on the real Oh My Pi (src/model-fallback.ts, on the cooldown machine and the
// chain resolver of @legion/pi-shared): only the real binary shows which model each request went
// to across a provider's refusal — the refusals the host walks its own chain for, the ones it
// leaves where they are, when the `context` hook puts the session back inside a turn already
// running, and that the transcript keeps the arming a relaunched worker restores.
// LEGION_TEST_OMP names the binary: the fork pin in the repository's .omp-pin, which
// CI's pi-legion job installs; on the devbox, `mise where <pin>`/bin/omp. A run without one skips,
// except on GitHub Actions, where a skip would hide the only run of the check on the host that
// ships it (GITHUB_ACTIONS, not CI: agent harnesses on the devbox export CI=true).
const omp = process.env.LEGION_TEST_OMP;
const onActions = process.env.GITHUB_ACTIONS === "true";

/** The pane's own models: the one every model role names, the one its chain steps to, and the
 * one in between, for the case whose chain has two steps. */
const STRONG = "standin/strong-model";
const MIDDLE = "standin/middle-model";
const WEAK = "standin/weak-model";

/** One stand-in serves them all, so a case tells them apart by the `model` of the request it
 * answers. The first model of the first provider is what every model role runs on
 * (`writeStandinProfile`), so the stronger model stays first. */
const PROVIDERS: readonly StandinProvider[] = [
  { provider: "standin", models: ["strong-model", "weak-model", "middle-model"] },
];

/**
 * The pane's overlay, in the shape INFRA's own operator overlay carries
 * (deploy/kubernetes/operator-route/overlay.yml, docs/kubernetes.md "Operator configuration"):
 * the model fallback on, the chain the case runs under, and no revert of the host's own — the
 * return is the plugin's.
 */
function overlay(chains: readonly string[]): readonly string[] {
  return [
    "retry:",
    "  modelFallback: true",
    "  fallbackRevertPolicy: never",
    "  fallbackChains:",
    ...chains,
  ];
}

/** One step: the chain every case but the two-refusal walk runs under. */
const ONE_STEP: readonly string[] = [`    ${STRONG}: [${WEAK}]`];

/** Two steps, each model keyed on its own: the host's own chain resolution keys a walk by the
 * model the session runs now (`resolveRetryFallbackChainKey`, `session/retry-fallback-chains.ts`
 * at the pinned tag), so the middle model needs a key of its own to be walked from. */
const TWO_STEPS: readonly string[] = [
  `    ${STRONG}: [${MIDDLE}, ${WEAK}]`,
  `    ${MIDDLE}: [${WEAK}]`,
];

/**
 * The first wait a refusal of one selector buys, through `LEGION_REFUSAL_COOLDOWN_MS`
 * (`fallbackCooldownLimits`): seconds here, an hour in a deployment, so every case below runs
 * inside the pi-legion job's ten minutes.
 */
const COOLDOWN_MS = 4_000;

/** The wait a worker killed mid-cooldown is resumed inside: long enough to cover omp's own
 * startup twice over, short enough for the return to come while the case watches. */
const RESUME_COOLDOWN_MS = 12_000;

const cleanup: Cleanup = [];
afterEach(async () => {
  for (const step of cleanup.splice(0).reverse()) await step();
});

/** A shell sleep the model asks for, which keeps one turn running while a cooldown runs out: the
 * requests of that same turn are the ones the `context` hook restores on. */
function sleeps(seconds: number): Block {
  return call("bash", { command: `sleep ${seconds}` });
}

/** The gateway's answer to one turn: the blocks, as an Anthropic event stream. */
function replies(blocks: readonly Block[], id: string): Response {
  return new Response(messageStream(blocks, id), {
    headers: { "content-type": "text/event-stream" },
  });
}

/** The gateway's answer to one turn: a content filter's refusal, after `refusal.text` when it
 * names any. */
function refuses(id: string, refusal: Refusal = {}): Response {
  return new Response(refusalStream(id, refusal), {
    headers: { "content-type": "text/event-stream" },
  });
}

/** The text of every user message in a Messages request, joined: which assignment the turn that
 * carried it was started by. */
function userText(request: Request): string {
  const messages: unknown[] = Array.isArray(request.body.messages) ? request.body.messages : [];
  return messages
    .flatMap((message) => {
      if (typeof message !== "object" || message === null) return [];
      if (!("role" in message) || message.role !== "user") return [];
      if (!("content" in message)) return [];
      return [JSON.stringify(message.content)];
    })
    .join("\n");
}

/** One turn the gateway served: the model it was sent to, and when it arrived. */
interface Served {
  readonly model: string;
  readonly at: number;
}

/** What a settled fallback pane left behind, beside every pane's own record. */
interface FallbackPane extends LegionPane {
  /** Every turn the gateway served: its model and its time, in order. */
  readonly served: readonly Served[];
  /** The model of every turn the gateway served, in order. */
  readonly models: () => string[];
  /** When the first turn after `since` went to the stronger model, or undefined when none did:
   * the moment the session came back. */
  readonly returnedAt: (since: number) => number | undefined;
  /** The cooldown states the pane recorded in its transcript, in order. */
  readonly cooldowns: () => Promise<FallbackCooldownState[]>;
}

/**
 * Runs one root architect's pane on the real Oh My Pi (`runLegionPane`) under the chain overlay,
 * its gateway answering each turn from `answer`, until every assignment's run has settled. A root
 * architect holds a Legion claim, which is all the model fallback gates on, and is the one role
 * the phase reminder leaves alone, so nothing but the case's own assignments starts a turn.
 */
async function fallbackPane(
  binary: string,
  options: {
    readonly prompt: readonly string[];
    /** Answers the turn the gateway was sent: `model` is the request's own model id, and `nth`
     * counts the turns of the whole pane, its second life included, from 1. */
    readonly answer: (model: string, nth: number, request: Request) => Response;
    /** `LEGION_REFUSAL_COOLDOWN_MS`; `COOLDOWN_MS` by default. */
    readonly cooldownMs?: number;
    /** The `fallbackChains` lines of the pane's overlay; `ONE_STEP` by default. */
    readonly chains?: readonly string[];
  }
): Promise<FallbackPane> {
  const served: Served[] = [];
  const pane = await runLegionPane(
    binary,
    [],
    {
      name: "fallback",
      role: "architect",
      tree: "FALL-1",
      issue: "FALL-1",
      prompt: options.prompt,
      config: overlay(options.chains ?? ONE_STEP),
      providers: PROVIDERS,
      env: {
        LEGION_REFUSAL_COOLDOWN_MS: String(options.cooldownMs ?? COOLDOWN_MS),
        // Well above every wait a case asks for, so the doubling is what decides them.
        LEGION_REFUSAL_COOLDOWN_MAX_MS: "600000",
      },
      answer: (url, body) => {
        if (url.pathname !== "/anthropic/v1/messages") return undefined;
        const model = String(body.model ?? "");
        served.push({ model, at: Date.now() });
        return options.answer(model, served.length, { path: url.pathname, body });
      },
    },
    cleanup
  );
  return {
    ...pane,
    served,
    models: () => served.map((turn) => turn.model),
    returnedAt: (since) =>
      served.find((turn) => turn.at >= since && turn.model === "strong-model")?.at,
    cooldowns: async () =>
      (await pane.transcriptEntries(LEGION_FALLBACK_ENTRY)) as FallbackCooldownState[],
  };
}

test.skipIf(omp === undefined && !onActions)(
  "a refusal before any output is retried on the chain's next model, which is the host's own walk",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    /** When the second assignment's turn must stop pinging the gateway and let the run settle:
     * well past the wait the refusal bought, so the case sees both sides of it. */
    let until = 0;
    const pane = await fallbackPane(omp, {
      // The second assignment is one long turn that asks the gateway every second, so the model
      // of each of its requests says where the session stood at that moment.
      prompt: ["Start FALL-1.", "Keep working."],
      answer: (_model, nth) => {
        if (nth === 1) {
          until = Date.now() + 2 * COOLDOWN_MS;
          return refuses("refusal_1");
        }
        if (nth === 2) return replies([{ type: "text", text: "Done." }], `msg_${nth}`);
        return Date.now() < until
          ? replies([sleeps(1)], `ping_${nth}`)
          : replies([{ type: "text", text: "Done." }], `msg_${nth}`);
      },
    });

    expect(pane.models().slice(0, 2)).toEqual(["strong-model", "weak-model"]);
    // One assignment, two requests: the host replayed the refused turn itself, which it may do
    // because nothing of it had been committed, rather than the plugin moving a later turn.
    expect(userText(pane.turns()[1] as Request)).toContain("Start FALL-1.");

    // The host walked its own chain, and the host's replay of the turn succeeded, so the settled
    // turn reads clean and only the fallback event itself says a filter refused the session. The
    // arming it leaves is a refusal's all the same: the same cooldown and the same doubling the
    // plugin's own walk takes.
    const states = await pane.cooldowns();
    expect(states[0]).toMatchObject({
      arming: { restoreTo: STRONG, cause: "refusal" },
      levels: { [STRONG]: 2 },
    });
    const dueAt = states[0]?.arming?.dueAt ?? 0;
    const waiting = pane.served.slice(1).filter((turn) => turn.at < dueAt);
    // The defect this case is the proof of: an arming the plugin read as anything but a refusal
    // waits on a suppression the host never applies for a classifier refusal — it pins the
    // fallback instead — so the session went straight back to the model that had just refused
    // it. Nothing goes back before the wait is over, and several requests fall inside it.
    expect(waiting.length).toBeGreaterThan(1);
    expect(waiting.map((turn) => turn.model)).not.toContain("strong-model");
    // And the session does come back, once the wait is over.
    expect(pane.returnedAt(dueAt)).toBeDefined();
  },
  180_000
);

test.skipIf(omp === undefined && !onActions)(
  "a refusal of the fallback model after the host's own walk steps down again and comes back to the model the session started on",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    /** When the turn that pings the gateway every second must stop and let the run settle: past
     * the doubled wait the second refusal bought. */
    let until = 0;
    const pane = await fallbackPane(omp, {
      chains: TWO_STEPS,
      prompt: ["Start FALL-1.", "Keep working."],
      answer: (_model, nth) => {
        // Refused before any output: the host walks its own chain to the middle model and
        // replays the turn there.
        if (nth === 1) return refuses("refusal_1");
        // The replay is refused after text, which the host leaves where it is: the plugin's own
        // walk takes it from the middle model to the weaker one.
        if (nth === 2) {
          until = Date.now() + 4 * COOLDOWN_MS;
          return refuses("refusal_2", { text: "Looking at it." });
        }
        return Date.now() < until
          ? replies([sleeps(1)], `ping_${nth}`)
          : replies([{ type: "text", text: "Done." }], `msg_${nth}`);
      },
    });

    expect(pane.models().slice(0, 3)).toEqual(["strong-model", "middle-model", "weak-model"]);
    const states = await pane.cooldowns();
    const armed = states.findLast((state) => state.arming !== undefined)?.arming;
    // The defect this case is the proof of: the plugin's own walk armed the return on the model
    // it was leaving, so the session came back to the middle of the chain and never to the model
    // it started on. The second step continues the host's own walk, so the return still points
    // at the stronger model, and the wait it bought is the doubled one of that model.
    expect(armed).toMatchObject({ restoreTo: STRONG, cause: "refusal" });
    expect(states.findLast((state) => state.arming !== undefined)?.levels).toEqual({
      [STRONG]: 3,
    });
    const refusedAt = pane.served[1]?.at ?? 0;
    const dueAt = armed?.dueAt ?? 0;
    expect(dueAt - refusedAt).toBeGreaterThanOrEqual(2 * COOLDOWN_MS);
    // Nothing went back before that wait, and what the session went back to is the stronger
    // model: no request of this pane ever went to the middle model again.
    const waiting = pane.served.slice(2).filter((turn) => turn.at < dueAt);
    expect(waiting.length).toBeGreaterThan(1);
    expect(waiting.map((turn) => turn.model)).not.toContain("strong-model");
    expect(pane.returnedAt(dueAt)).toBeDefined();
    expect(pane.models().slice(2)).not.toContain("middle-model");
  },
  240_000
);

test.skipIf(omp === undefined && !onActions)(
  "a refusal after streamed text, which the host leaves where it is, sends the next turn to the chain's next model",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    const pane = await fallbackPane(omp, {
      prompt: ["Start FALL-1.", "Carry on."],
      answer: (_model, nth) =>
        nth === 1
          ? refuses("refusal_1", { text: "Looking at it." })
          : replies([{ type: "text", text: "Done." }], `msg_${nth}`),
    });

    expect(pane.models()).toEqual(["strong-model", "weak-model"]);
    // The refused turn was replayed by nobody — committed text makes it unsafe to replay, so the
    // host ends the run there — and the next request is the next assignment's, on the model the
    // plugin's own walk moved the session to.
    expect(userText(pane.turns()[1] as Request)).toContain("Carry on.");
  },
  180_000
);

test.skipIf(omp === undefined && !onActions)(
  "the session is put back on the model it fell back from inside a turn already running, once the cooldown is over",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    const pane = await fallbackPane(omp, {
      prompt: ["Start FALL-1.", "Keep working."],
      answer: (_model, nth) => {
        if (nth === 1) return refuses("refusal_1", { text: "Looking at it." });
        // One long turn on the weaker model: the sleep outlasts the cooldown, so the requests
        // after it are the ones the `context` hook has something to do about.
        if (nth === 2) return replies([sleeps(6)], `msg_${nth}`);
        if (nth === 3) return replies([call("bash", { command: "echo next" })], `msg_${nth}`);
        return replies([{ type: "text", text: "Done." }], `msg_${nth}`);
      },
    });

    // The host chooses a request's model before it runs the `context` hook, so the hook's switch
    // is the model of the *next* request of that turn: the third request, sent after the sleep,
    // is still the weaker model, and the fourth is the model the session fell back from — inside
    // the same turn, which no new prompt and no host setting would have reached.
    expect(pane.models()).toEqual(["strong-model", "weak-model", "weak-model", "strong-model"]);
    const states = await pane.cooldowns();
    expect(states[0]).toMatchObject({
      arming: { restoreTo: STRONG, cause: "refusal" },
      levels: { [STRONG]: 2 },
    });
    // The return clears the arming and leaves the doubling where it stands.
    expect(states.at(-1)).toEqual({ levels: { [STRONG]: 1 } });
  },
  180_000
);

test.skipIf(omp === undefined && !onActions)(
  "a second refusal of the same model doubles its wait, so the return does not come at the first one",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    /** When the turn that pings every two seconds must stop and let the run settle. */
    let until = 0;
    const pane = await fallbackPane(omp, {
      prompt: ["Start FALL-1.", "Keep working.", "Keep going."],
      answer: (model, nth) => {
        // The first assignment is refused on the stronger model, which arms the base wait.
        if (nth === 1) return refuses("refusal_1", { text: "Looking at it." });
        // The second runs long enough for the return, and is refused again the moment it lands
        // on the stronger model: that refusal is the one whose wait has doubled.
        if (model === "strong-model" && until === 0) {
          until = Date.now() + 4 * COOLDOWN_MS;
          return refuses("refusal_2", { text: "Looking at it again." });
        }
        if (nth === 2) return replies([sleeps(6)], `msg_${nth}`);
        if (until === 0) return replies([call("bash", { command: "echo next" })], `msg_${nth}`);
        // The third assignment's turn keeps asking the gateway every two seconds, so the model
        // of each request says where the session stood at that moment.
        return Date.now() < until
          ? replies([sleeps(2)], `ping_${nth}`)
          : replies([{ type: "text", text: "Done." }], `msg_${nth}`);
      },
    });

    const secondRefusal = pane.served.filter((turn) => turn.model === "strong-model")[1]?.at;
    expect(secondRefusal).toBeDefined();
    const states = await pane.cooldowns();
    // The first refusal of the stronger model took the base wait and raised its level; the second
    // took twice the base and raised it again.
    expect(states[0]).toMatchObject({ levels: { [STRONG]: 2 } });
    const doubled = states.findLast((state) => state.arming !== undefined);
    expect(doubled?.levels).toEqual({ [STRONG]: 3 });
    const dueAt = doubled?.arming?.dueAt ?? 0;
    expect(dueAt - (secondRefusal ?? 0)).toBeGreaterThanOrEqual(2 * COOLDOWN_MS);
    expect(dueAt - (secondRefusal ?? 0)).toBeLessThan(2 * COOLDOWN_MS + 2_000);
    // The return came after the doubled wait, and no request went to the stronger model at the
    // first one.
    const returned = pane.returnedAt((secondRefusal ?? 0) + 1);
    expect(returned).toBeDefined();
    expect((returned ?? 0) - (secondRefusal ?? 0)).toBeGreaterThanOrEqual(2 * COOLDOWN_MS);
  },
  240_000
);

test.skipIf(omp === undefined && !onActions)(
  "a clean turn on the model the session came back to puts its wait back to the base",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    /** How many turns the stronger model has been sent: the first is refused, the second is the
     * clean turn that puts the wait back, the third is refused again. */
    let onStrong = 0;
    let until = 0;
    const pane = await fallbackPane(omp, {
      prompt: ["Start FALL-1.", "Keep working.", "Keep going.", "Carry on."],
      answer: (model, nth) => {
        if (model === "strong-model") {
          onStrong += 1;
          if (onStrong === 1) return refuses("refusal_1", { text: "Looking at it." });
          // The clean turn: it ends the second assignment's run on the stronger model.
          if (onStrong === 2) return replies([{ type: "text", text: "Back on it." }], `msg_${nth}`);
          if (onStrong === 3) {
            until = Date.now() + 2 * COOLDOWN_MS;
            return refuses("refusal_2", { text: "Looking at it again." });
          }
        }
        if (nth === 2) return replies([sleeps(6)], `msg_${nth}`);
        if (until === 0) return replies([call("bash", { command: "echo next" })], `msg_${nth}`);
        // The last assignment's turn asks the gateway every second, so the model of each request
        // says where the session stood at that moment.
        return Date.now() < until
          ? replies([sleeps(1)], `ping_${nth}`)
          : replies([{ type: "text", text: "Done." }], `msg_${nth}`);
      },
    });

    const secondRefusal = pane.served.filter((turn) => turn.model === "strong-model")[2]?.at;
    expect(secondRefusal).toBeDefined();
    const states = await pane.cooldowns();
    const armings = states.filter((state) => state.arming !== undefined);
    // Both refusals of the stronger model armed from level 1 and raised it to 2: the clean turn
    // between them put back what the first one raised, so the second bought the base wait again
    // rather than twice it.
    expect(armings.map((state) => state.levels[STRONG])).toEqual([2, 2]);
    const secondArming = states.findLastIndex((state) => state.arming !== undefined);
    expect(states[secondArming - 1]).toEqual({ levels: { [STRONG]: 1 } });
    const dueAt = states[secondArming]?.arming?.dueAt ?? 0;
    expect(dueAt - (secondRefusal ?? 0)).toBeGreaterThanOrEqual(COOLDOWN_MS);
    expect(dueAt - (secondRefusal ?? 0)).toBeLessThan(2 * COOLDOWN_MS);
    // And the session came back at that wait, before the doubled one it would have waited had
    // the clean turn counted for nothing.
    const returned = pane.returnedAt((secondRefusal ?? 0) + 1);
    expect(returned).toBeDefined();
    expect((returned ?? 0) - (secondRefusal ?? 0)).toBeLessThan(2 * COOLDOWN_MS);
  },
  240_000
);

test.skipIf(omp === undefined && !onActions)(
  "a pane whose process is gone resumes its own transcript's arming, and comes back no earlier than it says",
  async () => {
    if (omp === undefined) throw new Error("LEGION_TEST_OMP is unset on GitHub Actions");
    /** When the resumed life's long turn must stop pinging the gateway. */
    let until = 0;
    const pane = await fallbackPane(omp, {
      cooldownMs: RESUME_COOLDOWN_MS,
      // The second assignment is a tool-call turn: its own entries carry the arming written at
      // the end of the refused turn to disk, which is where the resumed life reads it.
      prompt: ["Start FALL-1.", "Note that."],
      answer: (_model, nth) => {
        if (nth === 1) return refuses("refusal_1", { text: "Looking at it." });
        if (nth === 2) return replies([call("bash", { command: "echo noted" })], `msg_${nth}`);
        if (nth === 3) return replies([{ type: "text", text: "Noted." }], `msg_${nth}`);
        return Date.now() < until
          ? replies([sleeps(2)], `ping_${nth}`)
          : replies([{ type: "text", text: "Done." }], `msg_${nth}`);
      },
    });

    const armed = (await pane.cooldowns()).at(-1)?.arming;
    expect(armed).toMatchObject({ restoreTo: STRONG, cause: "refusal" });
    const dueAt = armed?.dueAt ?? 0;
    expect(dueAt).toBeGreaterThan(Date.now());
    const firstLife = pane.served.length;

    // The daemon relaunches the worker: a fresh process whose only memory of the fallback is the
    // transcript it resumes.
    until = dueAt + 2 * 1_000;
    await pane.resume([], "Carry on after the relaunch.");

    const resumed = pane.served.slice(firstLife);
    expect(resumed.length).toBeGreaterThan(1);
    // Every request the resumed life sent before the restored due time went to the weaker model:
    // the arming was restored, not dropped, and its wait was not started again by the relaunch.
    expect(resumed.filter((turn) => turn.at < dueAt).map((turn) => turn.model)).not.toContain(
      "strong-model"
    );
    // And the return did come, which is what says the arming survived at all: a life that had
    // restored nothing would have stayed on the weaker model for good.
    const returned = pane.returnedAt(dueAt);
    expect(returned).toBeDefined();
    expect((returned ?? 0) - dueAt).toBeLessThan(RESUME_COOLDOWN_MS);
  },
  300_000
);
