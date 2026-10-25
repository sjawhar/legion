import { expect, test } from "bun:test";
import type { LegionState } from "@legion/contracts/legion-api";
import { createPhaseReminder } from "./phase-reminder";
import { DEFAULT_STALL_LIMITS } from "./stall-limits";

// A daemon that never answers the record check: the check fails open once its bound passes, and
// the read it started is aborted then rather than left open, one request per reminder.
test("the record check aborts a daemon read that outlives its bound, and reminds all the same", async () => {
  const handed: AbortSignal[] = [];
  const debugged: string[] = [];
  const reminder = createPhaseReminder({
    sendReminder: () => {},
    armTimer: () => {},
    saveEntry: () => {},
    claim: () => ({
      sessionID: "ses_implementer",
      tree: "LEGION-208",
      issue: "LEGION-208",
      role: "implementer",
      secret: "secret",
    }),
    readState: (signal) => {
      handed.push(signal);
      return new Promise<LegionState>(() => {});
    },
    publisher: () => undefined,
    project: "LEGION",
    limits: DEFAULT_STALL_LIMITS,
    log: { debug: (message) => debugged.push(message), warn: () => {} },
    recordBoundMs: 20,
  });
  reminder.apply({ kind: "assignment" });

  const continuation = await reminder.settle("Pushed.");

  expect(continuation).toBeString();
  expect(handed).toHaveLength(1);
  expect(handed[0]?.aborted).toBe(true);
  expect((handed[0]?.reason as Error).message).toBe("no answer within 20ms");
  expect(debugged).toEqual(["legion: the daemon did not answer the phase reminder's record check"]);
});
