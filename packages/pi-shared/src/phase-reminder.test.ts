import { expect, test } from "bun:test";
import type { LegionState } from "@legion/contracts/legion-api";
import { EnvoyApiError, type PublishInput } from "@legion/envoy-client/transport";
import { createPhaseReminder, type PhaseReminder } from "./phase-reminder";
import { type PhaseStallEntry, REMINDER } from "./phase-stall";
import { DEFAULT_STALL_LIMITS } from "./stall-limits";

const implementer = {
  sessionID: "ses_implementer",
  tree: "LEGION-208",
  issue: "LEGION-208",
  role: "implementer",
  secret: "secret",
  claimToken: "legion-legion-legion-208-implementer",
  generation: 1,
} as const;

/** A turn the worker ended by saying `text`, as `turnEndOf` classifies it on Oh My Pi. */
const said = (text: string) => ({ kind: "settle", lastAssistantText: text }) as const;

/** The daemon's state with this role's claim failed, which is its word that the session is
 * finished. Only the two fields the record check reads, cast because the state's own schema would
 * demand the whole daemon document for them; `rig` below reads its empty state the same way. */
const claimFailed = {
  issues: {
    "LEGION-208": {
      phase: "implementing",
      workers: { implementer: { claim: { state: "failed" } } },
    },
  },
} as unknown as LegionState;

// A daemon that never answers the record check: the check fails open once its bound passes, and
// the read it started is aborted then rather than left open, one request per reminder.
test("the record check aborts a daemon read that outlives its bound, and reminds all the same", async () => {
  const handed: AbortSignal[] = [];
  const debugged: string[] = [];
  const reminder = createPhaseReminder({
    sendReminder: () => {},
    armTimer: () => {},
    saveEntry: () => {},
    claim: () => implementer,
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

  const continuation = await reminder.settle(said("Pushed."));

  expect(continuation).toBeString();
  expect(handed).toHaveLength(1);
  expect(handed[0]?.aborted).toBe(true);
  expect((handed[0]?.reason as Error).message).toBe("no answer within 20ms");
  expect(debugged).toEqual(["legion: the daemon did not answer the phase reminder's record check"]);
});

interface ArmedTimer {
  readonly callback: () => void | Promise<void>;
  readonly delayMs: number;
}

interface Rig {
  readonly reminder: PhaseReminder;
  /** Every timer the runner armed, in order; a case fires one by calling its callback. */
  readonly timers: ArmedTimer[];
  /** Every reminder handed to `sendReminder`, delivered or not. */
  readonly sent: string[];
  readonly entries: PhaseStallEntry[];
  readonly published: PublishInput[];
  readonly warned: string[];
}

/** The runner over an open phase, with the architect told at the second delivered reminder. The
 * daemon holds no record for the issue unless a case names one, so every record check reminds. */
function rig(options: {
  readonly sendReminder?: (reminder: string) => void | Promise<void>;
  readonly publish?: (input: PublishInput) => Promise<unknown>;
  readonly state?: LegionState;
}): Rig {
  const timers: ArmedTimer[] = [];
  const sent: string[] = [];
  const entries: PhaseStallEntry[] = [];
  const published: PublishInput[] = [];
  const warned: string[] = [];
  const reminder = createPhaseReminder({
    sendReminder: (text) => {
      sent.push(text);
      return options.sendReminder?.(text);
    },
    armTimer: (callback, delayMs) => {
      timers.push({ callback, delayMs });
    },
    saveEntry: (entry) => entries.push(entry),
    claim: () => implementer,
    readState: async () => options.state ?? ({ issues: {} } as unknown as LegionState),
    publisher: () => async (input) => {
      published.push(input);
      return options.publish === undefined ? {} : options.publish(input);
    },
    project: "legion",
    limits: { ...DEFAULT_STALL_LIMITS, architectAfter: 2 },
    log: { debug: () => {}, warn: (message) => warned.push(message) },
  });
  reminder.apply({ kind: "assignment" });
  return { reminder, timers, sent, entries, published, warned };
}

/** Fires the last timer the runner armed. */
function fireLast(timers: readonly ArmedTimer[]): void | Promise<void> {
  const timer = timers.at(-1);
  if (timer === undefined) throw new Error("no timer is armed");
  return timer.callback();
}

// The acceptance of one run's notice, landing after the worker's own turn reset the count and a
// new run began, must not mark the new run as told: that would silence its notice at every
// threshold until the count reset again.
test("a notice accepted after its run ended acknowledges nothing of the next run", async () => {
  const listener = Promise.withResolvers<unknown>();
  const reached = Promise.withResolvers<void>();
  let attempt = 0;
  const { reminder, timers, entries, published } = rig({
    publish: () => {
      attempt += 1;
      if (attempt > 1) return Promise.resolve({});
      reached.resolve();
      return listener.promise;
    },
  });

  // The run's continuation, then the timer reminder that brings the count to the threshold, whose
  // publish the listener has not answered yet.
  expect(await reminder.settle(said("Pushed."))).toBeString();
  expect(await reminder.settle(said("Still here."))).toBeUndefined();
  const delivered = fireLast(timers);
  await reached.promise;
  const firstKey = published[0]?.idempotencyKey;
  expect(firstKey).toMatch(/^[0-9a-f-]{36}$/);

  // The worker answers in the reminder's own turn, and its next settle opens a new run.
  reminder.apply({ kind: "tool-call" });
  expect(await reminder.settle(said("Working on it."))).toBeString();

  listener.resolve({});
  await delivered;
  expect(entries.at(-1)).toEqual({
    state: "open",
    count: 1,
    reported: false,
    lastMessage: "Working on it.",
  });

  // The new run reaches the threshold and tells the architect under a key of its own.
  expect(await reminder.settle(said("Still here."))).toBeUndefined();
  await fireLast(timers);
  expect(published).toHaveLength(2);
  expect(published[1]?.idempotencyKey).toMatch(/^[0-9a-f-]{36}$/);
  expect(published[1]?.idempotencyKey).not.toBe(firstKey);
  expect(entries.at(-1)).toMatchObject({ count: 2, reported: true });
});

// A failure other than "no live session holds the role" is not dropped: the next delivered
// reminder of the run publishes the same notice again, under the same key, until one is accepted.
test("a notice the listener failed is published again by the run's next reminder, under the same key", async () => {
  let attempt = 0;
  const { reminder, timers, entries, published, warned } = rig({
    publish: async () => {
      attempt += 1;
      if (attempt > 1) return {};
      throw new EnvoyApiError({
        method: "POST",
        url: "http://listener.test/v1/messages/publish",
        status: 503,
        responseBody: JSON.stringify({ error: "the listener is restarting" }),
      });
    },
  });

  expect(await reminder.settle(said("Pushed."))).toBeString();
  expect(await reminder.settle(said("Still here."))).toBeUndefined();
  await fireLast(timers);
  expect(published).toHaveLength(1);
  expect(warned).toEqual(["legion: the architect was not told its worker is stalled"]);
  const failed = entries.at(-1);
  expect(failed).toMatchObject({ count: 2, reported: false });
  expect(failed?.reportKey).toBe(published[0]?.idempotencyKey);

  expect(await reminder.settle(said("Still here."))).toBeUndefined();
  await fireLast(timers);
  expect(published).toHaveLength(2);
  expect(published[1]?.idempotencyKey).toBe(failed?.reportKey);
  expect(entries.at(-1)).toMatchObject({ count: 3, reported: true, reportKey: failed?.reportKey });

  // Accepted once is told once.
  expect(await reminder.settle(said("Still here."))).toBeUndefined();
  await fireLast(timers);
  expect(published).toHaveLength(2);
});

// No turn starts on a reminder that never reached the worker, so no settle would arm the next
// one: the runner uncounts it and arms the next itself, and the reminding goes on.
test("a reminder the host failed to deliver is not counted, and the next is armed", async () => {
  let deliveries = 0;
  const { reminder, timers, sent, entries, published, warned } = rig({
    sendReminder: async () => {
      deliveries += 1;
      if (deliveries === 1) throw new Error("the bridge closed the socket");
    },
  });

  expect(await reminder.settle(said("Pushed."))).toBeString();
  expect(await reminder.settle(said("Still here."))).toBeUndefined();
  expect(timers).toHaveLength(1);
  await fireLast(timers);

  expect(warned).toEqual([
    "legion: a phase reminder did not reach this worker; it comes again later",
  ]);
  // Still at the continuation's count: the failed one counted for nothing, so it told no one.
  expect(entries.at(-1)).toMatchObject({ state: "open", count: 1 });
  expect(published).toEqual([]);
  expect(timers).toHaveLength(2);
  expect(timers[1]?.delayMs).toBe(DEFAULT_STALL_LIMITS.reminderMs);

  await fireLast(timers);
  expect(sent).toHaveLength(2);
  expect(sent[1]).toBe(sent[0]);
  expect(entries.at(-1)).toMatchObject({ state: "open", count: 2 });
  expect(published).toHaveLength(1);
});

// A turn that started while the failed write was in flight is a turn whose settle arms the next
// reminder, so the runner arms none of its own on top of it.
test("a failed delivery arms nothing once a turn has started meanwhile", async () => {
  const write = Promise.withResolvers<void>();
  const writing = Promise.withResolvers<void>();
  const { reminder, timers } = rig({
    sendReminder: () => {
      writing.resolve();
      return write.promise;
    },
  });

  expect(await reminder.settle(said("Pushed."))).toBeString();
  expect(await reminder.settle(said("Still here."))).toBeUndefined();
  const delivered = fireLast(timers);
  await writing.promise;
  reminder.turnStarted();
  write.reject(new Error("the bridge closed the socket"));
  await delivered;

  expect(timers).toHaveLength(1);
});

// The whole of dispatch://INFRA-264: every turn of that implementer was refused by the provider,
// and every reminder it was given at once bought another refused turn. So a refused turn earns no
// continuation; but the daemon's task for the phase ended with that turn, and below
// `launch_failure_limit` nothing fails the claim and nothing hands the task out again, so one
// delayed reminder follows it rather than silence (the architect's ruling, review round 2).
test("a refused turn below the limit reminds once, after the delay and not before", async () => {
  const { reminder, timers, sent, entries } = rig({});
  const recorded = entries.length;

  expect(
    await reminder.settle({ kind: "refused", message: "Overloaded: the model is busy" })
  ).toBeUndefined();

  expect(sent).toEqual([]);
  // Nothing of the state moved, the words the worker last said included: the provider's are not
  // the worker's, and the architect's notice quotes the worker.
  expect(entries).toHaveLength(recorded);
  expect(timers.map((timer) => timer.delayMs)).toEqual([DEFAULT_STALL_LIMITS.reminderMs]);

  await fireLast(timers);

  // Once: the delivered reminder arms no second timer, and the next turn settles its own.
  expect(sent).toEqual([REMINDER]);
  expect(timers).toHaveLength(1);

  // The next turn the worker does answer is reminded as any later settle of a run is: the first
  // reminder of this run has gone out, so this one comes from a timer too.
  expect(await reminder.settle(said("Pushed."))).toBeUndefined();
  expect(timers).toHaveLength(2);
});

// At `launch_failure_limit` the daemon fails the claim, which is its own word that this session
// is finished: the retry runs in a session of its own, in which the phase's task is given again.
// The timer the last refused turn armed still fires, and its record check drops the reminder, so
// nothing is written into a session nobody can act on.
test("a refused turn at the limit, whose claim the daemon failed, reminds nothing", async () => {
  const { reminder, timers, sent } = rig({ state: claimFailed });

  expect(
    await reminder.settle({ kind: "refused", message: "Refusal (reasoning_extraction)" })
  ).toBeUndefined();

  expect(timers).toHaveLength(1);
  await fireLast(timers);

  expect(sent).toEqual([]);
});

// The daemon failing this role's claim is its word that the session is finished: the retry runs
// in a session of its own, and a reminder written into this one reaches nobody who can act on it.
// It is what stops the reminding on Claude Code, whose turns this host does not settle.
test("no reminder is sent once the daemon has failed this role's claim", async () => {
  const timers: ArmedTimer[] = [];
  const sent: string[] = [];
  const debugged: string[] = [];
  const reminder = createPhaseReminder({
    sendReminder: (text) => {
      sent.push(text);
    },
    armTimer: (callback, delayMs) => {
      timers.push({ callback, delayMs });
    },
    saveEntry: () => {},
    claim: () => implementer,
    readState: async () => claimFailed,
    publisher: () => undefined,
    project: "legion",
    limits: DEFAULT_STALL_LIMITS,
    log: { debug: (message) => debugged.push(message), warn: () => {} },
  });
  // The continuation this first settle of a run would earn: the record check drops it.
  reminder.apply({ kind: "assignment" });
  expect(await reminder.settle(said("Still here."))).toBeUndefined();
  expect(timers).toEqual([]);

  // And the timer's reminder, for a run the worker has already been reminded of once: that one
  // is armed without a check and dropped by the check the timer runs.
  reminder.restore({ state: "open", count: 1, reported: false });
  expect(await reminder.settle(said("Still here."))).toBeUndefined();
  expect(timers).toHaveLength(1);
  await fireLast(timers);

  expect(sent).toEqual([]);
  expect(debugged).toEqual([
    "legion: the daemon has failed this claim; no reminder is sent",
    "legion: the daemon has failed this claim; no reminder is sent",
  ]);
});
