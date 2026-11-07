import { expect, test } from "bun:test";
import { createLegionDaemonClient } from "@legion/pi-shared/daemon-client";
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops";
import type { AgentEndEvent } from "@legion/pi-shared/pi-types";
import { createTurnOutcomeReporter, type TurnOutcomeLog } from "./turn-outcome";

const SESSION = "ses_implementer_208";

const implementer: ClaimCapability = {
  sessionID: SESSION,
  tree: "LEGION-208",
  issue: "LEGION-208",
  role: "implementer",
  secret: "U2VjcmV0",
  claimToken: "legion-legion-legion-208-implementer",
  generation: 3,
};

interface Rig {
  readonly report: (sessionID: string, event: AgentEndEvent) => Promise<void>;
  /** Every body posted to `claims/turn-outcome`, in the order the daemon received it. */
  readonly posted: Record<string, unknown>[];
  readonly debugged: string[];
  readonly warned: string[];
}

/** The reporter over a daemon that answers `answer` to every report, or refuses it. */
function rig(
  options: {
    readonly claim?: (sessionID: string) => ClaimCapability | undefined;
    readonly answer?: (attempt: number) => Response;
  } = {}
): Rig {
  const posted: Record<string, unknown>[] = [];
  const debugged: string[] = [];
  const warned: string[] = [];
  const stub = (async (input, init) => {
    const path = new URL(input.toString()).pathname;
    if (path !== "/legion/v1/claims/turn-outcome") throw new Error(`unexpected route ${path}`);
    posted.push(JSON.parse(String(init?.body)));
    return options.answer === undefined
      ? Response.json({ refusedRun: 0, failed: false })
      : options.answer(posted.length);
  }) as typeof fetch;
  const log: TurnOutcomeLog = {
    debug: (message) => debugged.push(message),
    warn: (message) => warned.push(message),
  };
  const client = createLegionDaemonClient("http://daemon.test", stub);
  const reporter = createTurnOutcomeReporter({
    daemon: () => client,
    claim: options.claim ?? ((sessionID) => (sessionID === SESSION ? implementer : undefined)),
    log,
  });
  return { report: reporter.report, posted, debugged, warned };
}

/** A run whose last assistant message settled normally. */
const clean: AgentEndEvent = {
  messages: [{ role: "user" }, { role: "assistant", stopReason: "stop" }, { role: "toolResult" }],
};

test("a turn the model answered is reported clean, under the claim's own credentials", async () => {
  const { report, posted } = rig();

  await report(SESSION, clean);

  expect(posted).toEqual([
    {
      claimToken: "legion-legion-legion-208-implementer",
      sessionId: SESSION,
      secret: "U2VjcmV0",
      generation: 3,
      sequence: 1,
      outcome: "clean",
    },
  ]);
});

// What dispatch://INFRA-264's implementer did on every turn: Oh My Pi ended the run with the last
// assistant message stopped at `error`, and the provider's words beside it.
test("a turn the provider refused is reported refused, with the provider's own message", async () => {
  const { report, posted } = rig();

  await report(SESSION, {
    messages: [
      { role: "assistant", stopReason: "stop" },
      { role: "assistant", stopReason: "error", errorMessage: "Overloaded: the model is busy" },
    ],
  });

  expect(posted[0]).toMatchObject({
    sequence: 1,
    outcome: "refused",
    message: "Overloaded: the model is busy",
  });
});

// The sequence is this producer's own: it is what keys the outcome the daemon stores, so two
// turns must never share one, and the order it gives them out is the order the turns ended in.
test("the sequence counts this plugin's own turns, from one and in order, within the generation", async () => {
  const { report, posted } = rig();

  await report(SESSION, clean);
  await report(SESSION, { messages: [{ role: "assistant", stopReason: "error" }] });
  await report(SESSION, clean);

  expect(posted.map((body) => body.sequence)).toEqual([1, 2, 3]);
  expect(posted.map((body) => body.outcome)).toEqual(["clean", "refused", "clean"]);
  expect(posted.every((body) => body.generation === 3)).toBe(true);
});

// Reports that overlap still take their numbers in the order their turns ended: the number is
// taken before anything is awaited, so the daemon can order them however late they arrive.
test("overlapping reports take their numbers in the order the turns ended", async () => {
  const { report, posted } = rig();

  const refused = report(SESSION, { messages: [{ role: "assistant", stopReason: "error" }] });
  const answered = report(SESSION, clean);
  await Promise.all([refused, answered]);

  expect(posted.map((body) => [body.sequence, body.outcome])).toEqual([
    [1, "refused"],
    [2, "clean"],
  ]);
});

// A `task` subagent loads an instance of this module of its own and registers no claim, and the
// pane's own instance is asked before its registration has answered.
test("a session holding no claim reports nothing and spends no sequence", async () => {
  const { report, posted } = rig();

  await report("ses_subagent", clean);
  await report(SESSION, clean);

  expect(posted).toHaveLength(1);
  expect(posted[0]).toMatchObject({ sequence: 1 });
});

// A refusal the daemon itself gives will not pass on a retry, so the turn goes uncounted and the
// pane says so once: a report dropped in silence is a claim that never fails.
test("a report the daemon refuses is warned about, and the next turn still reports", async () => {
  const { report, posted, warned } = rig({
    answer: (attempt) =>
      attempt === 1
        ? Response.json({ error: "sequence is required" }, { status: 400 })
        : Response.json({ refusedRun: 1, failed: false }),
  });

  await report(SESSION, { messages: [{ role: "assistant", stopReason: "error" }] });
  await report(SESSION, { messages: [{ role: "assistant", stopReason: "error" }] });

  expect(warned).toEqual(["legion: a turn's outcome did not reach the daemon"]);
  expect(posted.map((body) => body.sequence)).toEqual([1, 2]);
});
