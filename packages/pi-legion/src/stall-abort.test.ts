import { expect, test } from "bun:test";
import type { PiApi, SessionContext } from "@legion/pi-shared/pi-types";
import { DEFAULT_STALL_LIMITS, defaultCallBoundMs } from "@legion/pi-shared/stall-limits";
import {
  createStallWatchdog,
  type StallCall,
  type StallStepRecord,
  type StallWatchdog,
} from "@legion/pi-shared/stall-watchdog";
// The import below is this file's merge-base check: on the merge base there is no `./stall-abort`,
// so every test here fails to resolve rather than passing vacuously.
import { createStallAborter, STALL_RESTART_MESSAGE } from "./stall-abort";

const SECOND = 1_000;
const MINUTE = 60 * SECOND;
const START = 1_793_000_000_000;

type SentMessage = {
  readonly message: Parameters<PiApi["sendMessage"]>[0];
  readonly options: Parameters<PiApi["sendMessage"]>[1];
};

/**
 * The handler context as the host hands it over: `abort` answers `typeof` but is not an own
 * enumerable property — `Object.keys(ctx)` lists `ui` alone (measured 2026-11-07, `omp/18.16.0`
 * in `--mode rpc`) — so it is put on the prototype here, which is the shape a key list misses.
 */
function hostContext(abort?: (this: SessionContext) => void | Promise<void>): SessionContext {
  const prototype = abort === undefined ? {} : { abort };
  return Object.assign(Object.create(prototype), {
    cwd: "/workspace",
    ui: { notify: () => {} },
  }) as SessionContext;
}

test("the turn is ended with the host's own abort(), called on the context it came from", async () => {
  let abortedOn: unknown;
  // The host's `abort()` returns a Promise (measured), so the step is over only when that
  // Promise is: this one is held open until the case lets it settle.
  const host = Promise.withResolvers<void>();
  const context = hostContext(function abort(this: SessionContext) {
    abortedOn = this;
    return host.promise;
  });
  // The member the module reads is not one a key list holds, which is why it is asked for by
  // `typeof`.
  expect(Object.keys(context)).not.toContain("abort");

  const aborter = createStallAborter({ context: () => context, sendMessage: () => {} });
  // `abortTurn` may answer either way, and the detector awaits whatever it answers.
  const aborting = Promise.resolve(aborter.abortTurn());
  expect(abortedOn).toBe(context);
  expect(await Promise.race([aborting.then(() => "ended"), Promise.resolve("still ending")])).toBe(
    "still ending"
  );

  host.resolve();
  await aborting;
});

test("a host with no abort(), and a session with no context, throw so the ladder goes on", async () => {
  const hostless = createStallAborter({ context: () => hostContext(), sendMessage: () => {} });
  expect(hostless.abortTurn()).rejects.toThrow("this host has no ctx.abort()");

  const contextless = createStallAborter({ context: () => undefined, sendMessage: () => {} });
  expect(contextless.abortTurn()).rejects.toThrow("this session has no handler context");
});

test("the message the worker sends itself is steered, starts a turn and is no user message", () => {
  const sent: SentMessage[] = [];
  const aborter = createStallAborter({
    context: () => hostContext(),
    sendMessage: (message, options) => {
      sent.push({ message, options });
    },
  });

  aborter.sendMessage("Your turn was ended because its `bash` call (toolu_stuck) had no result.");

  expect(sent).toEqual([
    {
      message: {
        // Its own custom type: a `role: "user"` message is what the phase check reads as a new
        // assignment, so a worker restarting itself would reset that check's own count.
        customType: STALL_RESTART_MESSAGE,
        content: "Your turn was ended because its `bash` call (toolu_stuck) had no result.",
        display: true,
      },
      // Steered, and triggering a turn: the session is idle once the abort has ended the turn,
      // and the daemon sends nothing after a turn it counts as served.
      options: { deliverAs: "steer", triggerTurn: true },
    },
  ]);
});

test("a message the host refuses throws, so the step records it", () => {
  const aborter = createStallAborter({
    context: () => hostContext(),
    sendMessage: () => {
      throw new Error("this session is shutting down");
    },
  });
  expect(() => aborter.sendMessage("anything")).toThrow("this session is shutting down");
});

// ---------------------------------------------------------------------------------------------
// Driven by the detector itself: the second step of the ladder, against a faked host.
// ---------------------------------------------------------------------------------------------

interface LadderRig {
  readonly watchdog: StallWatchdog;
  readonly tick: (ms: number) => void;
  readonly steps: StallStepRecord[];
  readonly stopped: { readonly call: StallCall; readonly reason: string }[];
  readonly ended: string[];
  readonly aborts: number[];
  readonly sent: SentMessage[];
  readonly exitCodes: number[];
  readonly exitsWritten: number[];
}

/**
 * The detector with the real aborter over a faked host. `abortsTurn` is what this host's
 * `abort()` does: the measured one ends the turn, which the plugin's entry sees as an assistant
 * message that ended aborted and a `turn_end`; a host that cannot abort has no `abort` at all.
 */
function ladder(options: { abortsTurn: boolean; exitsRead?: number }): LadderRig {
  let clock = START;
  const steps: StallStepRecord[] = [];
  const stopped: { call: StallCall; reason: string }[] = [];
  const ended: string[] = [];
  const aborts: number[] = [];
  const sent: SentMessage[] = [];
  const exitCodes: number[] = [];
  const exitsWritten: number[] = [];
  let exits = options.exitsRead ?? 0;
  let watchdog: StallWatchdog | undefined;

  const context = options.abortsTurn
    ? hostContext(async () => {
        aborts.push(clock);
        // What the host does with the turn, as it was measured: the call's result comes back in
        // the same millisecond, the turn ends, and the abortedness is on the last assistant
        // message (`agent_end` carries no `stopReason`).
        watchdog?.assistantMessage({
          role: "assistant",
          stopReason: "aborted",
          errorMessage: "Interrupted by user",
        });
        watchdog?.turnEnded();
      })
    : hostContext();

  watchdog = createStallWatchdog({
    now: () => clock,
    limits: { ...DEFAULT_STALL_LIMITS, callBoundMs: defaultCallBoundMs("tester") },
    isIdle: () => false,
    processes: {
      tree: () => ({ read: true, processes: [] }),
      endCall: (call) => {
        ended.push(call.toolCallId);
        return 0;
      },
    },
    teller: {
      tellOverdue: () => ({ evidenceWritten: true, ownerTold: true }),
      recordStep: (record) => {
        steps.push(record);
      },
      tellStopped: (call, reason) => {
        stopped.push({ call, reason });
      },
    },
    aborter: createStallAborter({
      context: () => context,
      sendMessage: (message, messageOptions) => {
        sent.push({ message, options: messageOptions });
      },
    }),
    exits: {
      read: () => exits,
      write: (count) => {
        exitsWritten.push(count);
        exits = count;
        return true;
      },
    },
    exitProcess: (code) => {
      exitCodes.push(code);
    },
    log: { debug: () => {}, warn: () => {} },
  });

  return {
    watchdog,
    tick: (ms) => {
      clock += ms;
    },
    steps,
    stopped,
    ended,
    aborts,
    sent,
    exitCodes,
    exitsWritten,
  };
}

/** One call made and handed to its tool: the shape this issue is about, a PTY shell call that
 * writes nothing after its `tool_execution_start` (measured twice). */
function running(watchdog: StallWatchdog, toolCallId = "toolu_stuck"): void {
  const input = { command: "sleep 100000", pty: true };
  watchdog.callRequested({ toolName: "bash", toolCallId, input });
  watchdog.executionStarted({ toolName: "bash", toolCallId, input });
}

test("the second step ends the turn and sends the session a message naming the ended call", async () => {
  const rig = ladder({ abortsTurn: true });
  running(rig.watchdog);

  // The first step, which does not end the call.
  rig.tick(10 * MINUTE + SECOND);
  await rig.watchdog.sweep();
  expect(rig.ended).toEqual(["toolu_stuck"]);
  expect(rig.aborts).toEqual([]);

  // The second, a step gap later, because the first did not end it.
  rig.tick(2 * MINUTE);
  await rig.watchdog.sweep();
  expect(rig.aborts).toHaveLength(1);
  expect(rig.steps.map((step) => step.step)).toEqual(["end-processes", "abort-turn"]);
  expect(rig.steps[1]?.outcome).toContain("the turn was ended with the host's abort()");
  expect(rig.steps[1]?.outcome).toContain("the session was sent the message");

  // The message names the call that was ended, and the tool it was a call of.
  expect(rig.sent).toHaveLength(1);
  const message = rig.sent[0]?.message as { customType: string; content: string };
  expect(message.customType).toBe(STALL_RESTART_MESSAGE);
  expect(message.content).toContain("toolu_stuck");
  expect(message.content).toContain("`bash`");
  expect(rig.sent[0]?.options).toEqual({ deliverAs: "steer", triggerTurn: true });

  // A turn the abort ended never reaches the exit: the call's record went with the turn.
  expect(rig.watchdog.openCalls()).toEqual([]);
  rig.tick(2 * MINUTE);
  await rig.watchdog.sweep();
  expect(rig.exitCodes).toEqual([]);
  expect(rig.exitsWritten).toEqual([]);
  expect(rig.steps.map((step) => step.step)).toEqual(["end-processes", "abort-turn"]);
});

test("a call that ends after the first step never reaches the abort", async () => {
  const rig = ladder({ abortsTurn: true });
  running(rig.watchdog);

  rig.tick(10 * MINUTE + SECOND);
  await rig.watchdog.sweep();
  expect(rig.ended).toEqual(["toolu_stuck"]);

  // The first step ended the call's own process, so the host closes the call.
  rig.watchdog.executionEnded({ toolCallId: "toolu_stuck" });

  rig.tick(2 * MINUTE);
  await rig.watchdog.sweep();
  rig.tick(2 * MINUTE);
  await rig.watchdog.sweep();
  expect(rig.aborts).toEqual([]);
  expect(rig.sent).toEqual([]);
  expect(rig.exitCodes).toEqual([]);
  expect(rig.steps.map((step) => step.step)).toEqual(["end-processes"]);
});

test("an abort the host cannot make is recorded, and the ladder goes on to the last step", async () => {
  const rig = ladder({ abortsTurn: false });
  running(rig.watchdog);

  rig.tick(10 * MINUTE + SECOND);
  await rig.watchdog.sweep();
  rig.tick(2 * MINUTE);
  await rig.watchdog.sweep();

  // The abort failed, and the message that would have restarted the work still went out, so the
  // turn is not left ended with nothing to restart it.
  expect(rig.steps[1]?.outcome).toContain(
    "the host's abort() failed: this host has no ctx.abort()"
  );
  expect(rig.sent).toHaveLength(1);
  expect(rig.aborts).toEqual([]);

  // And the ladder has a step left: the call is still open, so the next gap reaches the exit.
  rig.tick(2 * MINUTE);
  await rig.watchdog.sweep();
  expect(rig.steps.map((step) => step.step)).toEqual([
    "end-processes",
    "abort-turn",
    "exit-process",
  ]);
  expect(rig.exitsWritten).toEqual([1]);
  expect(rig.exitCodes).toEqual([1]);
  expect(rig.stopped).toEqual([]);
});
